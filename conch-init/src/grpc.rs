use std::collections::HashMap;
use std::io;
use std::net::SocketAddr;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::atomic::Ordering;

use futures_core::Stream;
#[cfg(unix)]
use std::os::unix::process::ExitStatusExt;
use tokio::fs;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;
use tokio::process::Command;
use tokio_stream::wrappers::ReceiverStream;
use tonic::codegen::InterceptedService;
use tonic::transport::Server;
use tonic::{Request, Response, Status};
use tracing::{error, info, warn};

use crate::auth::AuthInterceptor;
use crate::constants::{AGENT_DESCRIPTOR_SET, AGENT_FILE_CHUNK_BYTES, FILE_PERM, SERVER_PORT};
use crate::pb::agent_service_server::{AgentService, AgentServiceServer};
use crate::pb::{
    CheckReply, Empty, FileChunk, GetFileRequest, PostFilesResponse, StartProcessRequest,
    StartProcessResponse,
};
use crate::state::AgentState;
use crate::util::{clean_agent_filepath, clean_path_string};

#[derive(Clone)]
struct AgentServerImpl {
    _state: AgentState,
}

#[tonic::async_trait]
impl AgentService for AgentServerImpl {
    async fn health_check(&self, _request: Request<Empty>) -> Result<Response<CheckReply>, Status> {
        info!("Received health check request");
        Ok(Response::new(CheckReply {
            message: "OK".into(),
        }))
    }

    async fn start_process(
        &self,
        request: Request<StartProcessRequest>,
    ) -> Result<Response<StartProcessResponse>, Status> {
        let req = request.into_inner();
        info!(cmd = %req.cmd, cwd = %req.cwd, has_content = !req.content.is_empty(), "Received start process request");

        let work_dir = match prepare_work_dir(&req.cwd).await {
            Ok(path) => path,
            Err(err) => {
                return Ok(Response::new(error_response(format!(
                    "failed to prepare working directory: {err}"
                ))));
            }
        };

        let script_path = match write_script(&work_dir, &req.cmd, &req.content).await {
            Ok(path) => path,
            Err(err) => {
                return Ok(Response::new(error_response(format!(
                    "failed to write script file: {err}"
                ))));
            }
        };

        let mut args = req.args.clone();
        if args.is_empty() {
            if let Some(path) = script_path.as_ref() {
                args.push(path.to_string_lossy().to_string());
            }
        }

        let result = execute_cmd(&req.cmd, &args, &work_dir, &req.env).await;
        if let Some(path) = script_path {
            if let Err(err) = fs::remove_file(&path).await {
                if err.kind() != io::ErrorKind::NotFound {
                    warn!(path = %path.display(), error = %err, "failed to remove temporary script");
                }
            }
        }

        match result {
            Ok((stdout, stderr, exit_code)) => Ok(Response::new(StartProcessResponse {
                stdout,
                stderr,
                exit_code,
                error: String::new(),
            })),
            Err(ExecResult {
                stdout,
                stderr,
                exit_code,
                error,
            }) => Ok(Response::new(StartProcessResponse {
                stdout,
                stderr,
                exit_code,
                error: format!("failed to execute process: {error}"),
            })),
        }
    }

    async fn post_file_stream(
        &self,
        request: Request<tonic::Streaming<FileChunk>>,
    ) -> Result<Response<PostFilesResponse>, Status> {
        let mut stream = request.into_inner();
        let mut target_path: Option<PathBuf> = None;
        let mut temp_path: Option<PathBuf> = None;
        let mut file: Option<fs::File> = None;
        let mut total_bytes: i64 = 0;
        let committed = false;

        while let Some(chunk) = stream.message().await? {
            if chunk.content.len() > AGENT_FILE_CHUNK_BYTES {
                cleanup_upload(file.take(), temp_path.as_deref(), committed).await;
                return Ok(Response::new(PostFilesResponse {
                    uploaded_count: 0,
                    error: format!(
                        "file chunk exceeds maximum size {} bytes",
                        AGENT_FILE_CHUNK_BYTES
                    ),
                }));
            }

            if file.is_none() {
                let cleaned = match clean_agent_filepath(&chunk.filepath, "stream upload") {
                    Ok(path) => path,
                    Err(err) => {
                        return Ok(Response::new(PostFilesResponse {
                            uploaded_count: 0,
                            error: err,
                        }));
                    }
                };
                if let Some(parent) = cleaned.parent() {
                    if let Err(err) = fs::create_dir_all(parent).await {
                        return Ok(Response::new(PostFilesResponse {
                            uploaded_count: 0,
                            error: format!(
                                "failed to create parent directory for {}: {err}",
                                cleaned.display()
                            ),
                        }));
                    }
                }
                let created = match create_temp_file(
                    cleaned.parent().unwrap_or_else(|| Path::new(".")),
                    ".conch-upload-",
                    FILE_PERM,
                )
                .await
                {
                    Ok(v) => v,
                    Err(err) => {
                        return Ok(Response::new(PostFilesResponse {
                            uploaded_count: 0,
                            error: format!(
                                "failed to create temporary upload file for {}: {err}",
                                cleaned.display()
                            ),
                        }));
                    }
                };
                temp_path = Some(created.0);
                file = Some(created.1);
                target_path = Some(cleaned);
            } else if !chunk.filepath.is_empty() {
                let changed = PathBuf::from(clean_path_string(&chunk.filepath));
                if target_path.as_ref() != Some(&changed) {
                    cleanup_upload(file.take(), temp_path.as_deref(), committed).await;
                    return Ok(Response::new(PostFilesResponse {
                        uploaded_count: 0,
                        error: "filepath changed during stream upload".into(),
                    }));
                }
            }

            if !chunk.content.is_empty() {
                if let Some(f) = file.as_mut() {
                    if let Err(err) = f.write_all(&chunk.content).await {
                        let path = target_path
                            .as_ref()
                            .map(|p| p.display().to_string())
                            .unwrap_or_default();
                        cleanup_upload(file.take(), temp_path.as_deref(), committed).await;
                        return Ok(Response::new(PostFilesResponse {
                            uploaded_count: 0,
                            error: format!("failed to write file {path}: {err}"),
                        }));
                    }
                    total_bytes += chunk.content.len() as i64;
                }
            }
        }

        let Some(mut f) = file.take() else {
            return Ok(Response::new(PostFilesResponse {
                uploaded_count: 0,
                error: "no file chunks provided".into(),
            }));
        };
        let target = target_path.expect("target path exists when file exists");
        let temp = temp_path.expect("temp path exists when file exists");
        if let Err(err) = f.flush().await {
            cleanup_upload(Some(f), Some(&temp), committed).await;
            return Ok(Response::new(PostFilesResponse {
                uploaded_count: 0,
                error: format!("failed to close uploaded file {}: {err}", target.display()),
            }));
        }
        drop(f);
        if let Err(err) = fs::rename(&temp, &target).await {
            cleanup_upload(None, Some(&temp), committed).await;
            return Ok(Response::new(PostFilesResponse {
                uploaded_count: 0,
                error: format!("failed to commit uploaded file {}: {err}", target.display()),
            }));
        }
        info!(file = %target.display(), size = total_bytes, "Successfully uploaded file by stream");
        Ok(Response::new(PostFilesResponse {
            uploaded_count: 1,
            error: String::new(),
        }))
    }

    type GetFileStreamStream =
        std::pin::Pin<Box<dyn Stream<Item = Result<FileChunk, Status>> + Send + 'static>>;

    async fn get_file_stream(
        &self,
        request: Request<GetFileRequest>,
    ) -> Result<Response<Self::GetFileStreamStream>, Status> {
        let req = request.into_inner();
        let cleaned = clean_agent_filepath(&req.filepath, "stream file retrieval")
            .map_err(Status::invalid_argument)?;
        let info = fs::metadata(&cleaned).await.map_err(|err| {
            if err.kind() == io::ErrorKind::NotFound {
                Status::not_found(format!("file not found: {}", cleaned.display()))
            } else {
                Status::not_found(format!("failed to stat file {}: {err}", cleaned.display()))
            }
        })?;
        if info.is_dir() {
            return Err(Status::invalid_argument(format!(
                "path is a directory: {}",
                cleaned.display()
            )));
        }

        let (tx, rx) = tokio::sync::mpsc::channel(4);
        tokio::spawn(async move {
            let mut file = match fs::File::open(&cleaned).await {
                Ok(file) => file,
                Err(err) => {
                    let _ = tx
                        .send(Err(Status::not_found(format!(
                            "failed to open file {}: {err}",
                            cleaned.display()
                        ))))
                        .await;
                    return;
                }
            };
            let mut first = true;
            let mut buf = vec![0_u8; AGENT_FILE_CHUNK_BYTES];
            loop {
                match file.read(&mut buf).await {
                    Ok(0) => {
                        info!(file = %cleaned.display(), size = info.len(), "Successfully streamed file");
                        break;
                    }
                    Ok(n) => {
                        let filepath = if first {
                            first = false;
                            cleaned.to_string_lossy().to_string()
                        } else {
                            String::new()
                        };
                        let chunk = FileChunk {
                            filepath,
                            content: buf[..n].to_vec(),
                        };
                        if tx.send(Ok(chunk)).await.is_err() {
                            break;
                        }
                    }
                    Err(err) => {
                        let _ = tx.send(Err(Status::internal(err.to_string()))).await;
                        break;
                    }
                }
            }
        });

        Ok(Response::new(
            Box::pin(ReceiverStream::new(rx)) as Self::GetFileStreamStream
        ))
    }
}

pub async fn start_grpc_server_async(state: AgentState) -> io::Result<()> {
    let addr: SocketAddr = SERVER_PORT.parse().expect("valid server address");
    let listener = TcpListener::bind(addr).await?;
    let service = AgentServiceServer::new(AgentServerImpl {
        _state: state.clone(),
    });
    let intercepted: InterceptedService<_, AuthInterceptor> = InterceptedService::new(
        service,
        AuthInterceptor {
            auth: state.auth.clone(),
        },
    );
    let incoming = tokio_stream::wrappers::TcpListenerStream::new(listener);
    let reflection = tonic_reflection::server::Builder::configure()
        .register_encoded_file_descriptor_set(AGENT_DESCRIPTOR_SET)
        .build_v1()
        .map_err(|err| io::Error::new(io::ErrorKind::Other, err.to_string()))?;
    state.mark_grpc_ready();
    info!(port = SERVER_PORT, "gRPC server listening");
    tokio::spawn(async move {
        if let Err(err) = Server::builder()
            .add_service(reflection)
            .add_service(intercepted)
            .serve_with_incoming(incoming)
            .await
        {
            state.safe.store(false, Ordering::SeqCst);
            state.mark_grpc_not_ready();
            error!(error = %err, "gRPC server error");
        }
    });
    Ok(())
}

async fn prepare_work_dir(cwd: &str) -> io::Result<PathBuf> {
    let path = if cwd.is_empty() {
        std::env::var_os("HOME")
            .map(PathBuf::from)
            .unwrap_or_else(|| PathBuf::from("/root"))
    } else {
        PathBuf::from(cwd)
    };
    fs::create_dir_all(&path).await?;
    Ok(path)
}

async fn write_script(work_dir: &Path, cmd: &str, content: &str) -> io::Result<Option<PathBuf>> {
    if content.is_empty() {
        return Ok(None);
    }
    let ext = match cmd {
        "python" | "python3" | "python2" => ".py",
        "node" | "nodejs" => ".js",
        "bash" | "sh" | "zsh" => ".sh",
        "fish" => ".sh",
        "lua" => ".lua",
        "ruby" | "rb" => ".rb",
        _ => ".py",
    };
    let (path, mut file) = create_temp_file(work_dir, "conch-script-", FILE_PERM).await?;
    let final_path = path.with_extension(ext.trim_start_matches('.'));
    if path != final_path {
        drop(file);
        fs::rename(&path, &final_path).await?;
        file = fs::OpenOptions::new().write(true).open(&final_path).await?;
    }
    file.write_all(content.as_bytes()).await?;
    file.flush().await?;
    Ok(Some(final_path))
}

async fn create_temp_file(dir: &Path, prefix: &str, mode: u32) -> io::Result<(PathBuf, fs::File)> {
    for i in 0..1000_u32 {
        let path = dir.join(format!("{}{}-{}", prefix, unsafe { libc::getpid() }, i));
        let file = fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&path)
            .await;
        match file {
            Ok(file) => {
                set_mode(&path, mode).await?;
                return Ok((path, file));
            }
            Err(err) if err.kind() == io::ErrorKind::AlreadyExists => continue,
            Err(err) => return Err(err),
        }
    }
    Err(io::Error::new(
        io::ErrorKind::AlreadyExists,
        "failed to allocate temporary file",
    ))
}

async fn set_mode(path: &Path, mode: u32) -> io::Result<()> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let mut perm = fs::metadata(path).await?.permissions();
        perm.set_mode(mode);
        fs::set_permissions(path, perm).await
    }
    #[cfg(not(unix))]
    {
        let _ = (path, mode);
        Ok(())
    }
}

#[derive(Debug)]
struct ExecResult {
    stdout: String,
    stderr: String,
    exit_code: i32,
    error: String,
}

async fn execute_cmd(
    cmd_name: &str,
    args: &[String],
    work_dir: &Path,
    env_map: &HashMap<String, String>,
) -> Result<(String, String, i32), ExecResult> {
    if cmd_name.is_empty() {
        return Err(ExecResult {
            stdout: String::new(),
            stderr: String::new(),
            exit_code: -1,
            error: "command is required".into(),
        });
    }
    let mut cmd = Command::new(cmd_name);
    cmd.args(args)
        .current_dir(work_dir)
        .envs(env_map)
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true);
    match cmd.output().await {
        Ok(output) => {
            let stdout = String::from_utf8_lossy(&output.stdout).to_string();
            let stderr = String::from_utf8_lossy(&output.stderr).to_string();
            let exit_code = output.status.code().unwrap_or(-1);
            if let Some(signal) = process_signal(&output.status) {
                return Err(ExecResult {
                    stdout,
                    stderr,
                    exit_code,
                    error: format!("process terminated by signal {signal}"),
                });
            }
            Ok((stdout, stderr, exit_code))
        }
        Err(err) => Err(ExecResult {
            stdout: String::new(),
            stderr: String::new(),
            exit_code: -1,
            error: err.to_string(),
        }),
    }
}

fn process_signal(status: &std::process::ExitStatus) -> Option<i32> {
    #[cfg(unix)]
    {
        status.signal()
    }
    #[cfg(not(unix))]
    {
        let _ = status;
        None
    }
}

fn error_response(err: String) -> StartProcessResponse {
    error!(message = %err, "Process error");
    StartProcessResponse {
        stdout: String::new(),
        stderr: String::new(),
        exit_code: -1,
        error: err,
    }
}

async fn cleanup_upload(file: Option<fs::File>, temp_path: Option<&Path>, committed: bool) {
    drop(file);
    if !committed {
        if let Some(path) = temp_path {
            if let Err(err) = fs::remove_file(path).await {
                if err.kind() != io::ErrorKind::NotFound {
                    warn!(path = %path.display(), error = %err, "failed to remove temporary upload file");
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use std::collections::HashMap;
    use std::fs;
    use std::io;
    use std::path::{Path, PathBuf};

    use super::{AgentServerImpl, execute_cmd, prepare_work_dir, write_script};
    use crate::pb::GetFileRequest;
    use crate::pb::agent_service_server::AgentService;
    use crate::state::AgentState;
    use tonic::Request;

    #[tokio::test]
    async fn execute_cmd_returns_nonzero_exit_without_error() {
        let env = HashMap::new();
        let result = execute_cmd(
            "/bin/sh",
            &["-c".into(), "printf out; printf err >&2; exit 7".into()],
            Path::new("/tmp"),
            &env,
        )
        .await
        .expect("non-zero exit should be a normal process result");

        assert_eq!(result.0, "out");
        assert_eq!(result.1, "err");
        assert_eq!(result.2, 7);
    }

    #[tokio::test]
    async fn execute_cmd_returns_error_for_signal_exit() {
        let env = HashMap::new();
        let err = execute_cmd(
            "/bin/sh",
            &["-c".into(), "kill -TERM $$".into()],
            Path::new("/tmp"),
            &env,
        )
        .await
        .expect_err("signal termination should be reported as an execution error");

        assert_eq!(err.exit_code, -1);
        assert!(err.error.contains("process terminated by signal"));
    }

    #[tokio::test]
    async fn execute_cmd_supports_cwd_and_env() {
        let dir = temp_dir("execute-cwd-env").expect("temp dir");
        let env = HashMap::from([("CONCH_TEST_VALUE".to_string(), "from-env".to_string())]);

        let result = execute_cmd(
            "/bin/sh",
            &[
                "-c".into(),
                "pwd; printf \"$CONCH_TEST_VALUE\"; printf err >&2".into(),
            ],
            &dir,
            &env,
        )
        .await
        .expect("process should run");

        assert_eq!(result.2, 0);
        assert!(result.0.contains(dir.to_string_lossy().as_ref()));
        assert!(result.0.ends_with("from-env"));
        assert_eq!(result.1, "err");
    }

    #[tokio::test]
    async fn execute_cmd_kills_child_when_cancelled() {
        let env = HashMap::new();
        let dir = temp_dir("execute-cancel").expect("temp dir");
        let marker = dir.join("child-survived");
        let script = format!("sleep 1; printf survived > {}", marker.display());

        let handle = tokio::spawn(async move {
            execute_cmd("/bin/sh", &["-c".into(), script], Path::new("/tmp"), &env).await
        });
        tokio::time::sleep(std::time::Duration::from_millis(100)).await;
        handle.abort();
        let _ = handle.await;
        tokio::time::sleep(std::time::Duration::from_millis(1200)).await;

        assert!(
            !marker.exists(),
            "cancelled process remained alive and wrote marker"
        );
    }

    #[tokio::test]
    async fn write_script_uses_command_extension_and_can_be_cleaned() {
        let dir = temp_dir("write-script").expect("temp dir");
        let script = write_script(&dir, "sh", "printf script-ok")
            .await
            .expect("write script")
            .expect("script path");

        assert_eq!(script.extension().and_then(|v| v.to_str()), Some("sh"));
        assert!(
            fs::read_to_string(&script)
                .expect("script content")
                .contains("script-ok")
        );

        fs::remove_file(&script).expect("remove script");
        assert_no_conch_scripts(&dir);
    }

    #[tokio::test]
    async fn prepare_work_dir_creates_requested_directory() {
        let dir = temp_dir("prepare-workdir")
            .expect("temp dir")
            .join("nested");
        let prepared = prepare_work_dir(dir.to_string_lossy().as_ref())
            .await
            .expect("prepare work dir");

        assert_eq!(prepared, dir);
        assert!(prepared.is_dir());
    }

    #[tokio::test]
    async fn get_file_stream_reports_go_compatible_not_found_error() {
        let server = AgentServerImpl {
            _state: AgentState::new(),
        };
        let missing = temp_dir("get-missing")
            .expect("temp dir")
            .join("missing.txt");

        let result = server
            .get_file_stream(Request::new(GetFileRequest {
                filepath: missing.to_string_lossy().to_string(),
            }))
            .await;
        let err = match result {
            Ok(_) => panic!("missing file should fail before stream starts"),
            Err(err) => err,
        };

        assert_eq!(err.code(), tonic::Code::NotFound);
        assert_eq!(
            err.message(),
            format!("file not found: {}", missing.display())
        );
    }

    fn temp_dir(name: &str) -> io::Result<PathBuf> {
        let dir = std::env::temp_dir().join(format!(
            "conch-init-test-{name}-{}-{}",
            std::process::id(),
            unique_suffix()
        ));
        fs::create_dir_all(&dir)?;
        Ok(dir)
    }

    fn unique_suffix() -> u128 {
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .expect("time")
            .as_nanos()
    }

    fn assert_no_conch_scripts(dir: &Path) {
        let entries = fs::read_dir(dir)
            .expect("read work dir")
            .flatten()
            .map(|entry| entry.file_name().to_string_lossy().to_string())
            .collect::<Vec<_>>();
        assert!(
            entries
                .iter()
                .all(|name| !name.starts_with("conch-script-")),
            "temporary scripts remain: {entries:?}"
        );
    }
}
