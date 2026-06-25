fn main() -> Result<(), Box<dyn std::error::Error>> {
    tonic_build::configure()
        .build_server(true)
        .build_client(true)
        .file_descriptor_set_path(
            std::path::PathBuf::from(std::env::var("OUT_DIR")?).join("agent_descriptor.bin"),
        )
        .compile_protos(&["../api/agent.proto"], &["../api"])?;
    println!("cargo:rerun-if-changed=../api/agent.proto");
    Ok(())
}
