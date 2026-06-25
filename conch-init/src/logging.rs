use std::fmt;
use std::fs::{File, OpenOptions};
use std::io::{self, Write};
use std::path::Path;
use std::sync::{Arc, OnceLock, RwLock};

use chrono::Local;
use tracing::Subscriber;
use tracing::field::{Field, Visit};
use tracing_subscriber::fmt::format::Writer;
use tracing_subscriber::fmt::{FmtContext, FormatEvent, FormatFields};
use tracing_subscriber::registry::LookupSpan;

static LOG_SANDBOX_ID: OnceLock<Arc<RwLock<String>>> = OnceLock::new();
static LOG_WRITER: OnceLock<Arc<RwLock<LogDestination>>> = OnceLock::new();

pub fn init() {
    let _ = LOG_SANDBOX_ID.set(Arc::new(RwLock::new(String::new())));
    let writer = Arc::new(RwLock::new(LogDestination::Stderr));
    let _ = LOG_WRITER.set(writer.clone());
    tracing_subscriber::fmt()
        .with_writer(move || SharedLogWriter {
            destination: writer.clone(),
        })
        .event_format(GoStyleEventFormatter)
        .fmt_fields(GoStyleFieldFormatter)
        .init();
}

pub fn use_file(path: &Path) -> io::Result<()> {
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let file = OpenOptions::new().create(true).append(true).open(path)?;
    if let Some(writer) = LOG_WRITER.get() {
        let mut guard = writer
            .write()
            .map_err(|_| io::Error::other("log writer lock poisoned"))?;
        *guard = LogDestination::File(file);
    }
    Ok(())
}

pub fn set_sandbox_id(sandbox_id: &str) {
    if let Some(global) = LOG_SANDBOX_ID.get() {
        if let Ok(mut guard) = global.write() {
            *guard = sandbox_id.to_string();
        }
    }
}

enum LogDestination {
    Stderr,
    File(File),
}

struct SharedLogWriter {
    destination: Arc<RwLock<LogDestination>>,
}

impl Write for SharedLogWriter {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        let mut guard = self
            .destination
            .write()
            .map_err(|_| io::Error::other("log writer lock poisoned"))?;
        match &mut *guard {
            LogDestination::Stderr => io::stderr().write(buf),
            LogDestination::File(file) => file.write(buf),
        }
    }

    fn flush(&mut self) -> io::Result<()> {
        let mut guard = self
            .destination
            .write()
            .map_err(|_| io::Error::other("log writer lock poisoned"))?;
        match &mut *guard {
            LogDestination::Stderr => io::stderr().flush(),
            LogDestination::File(file) => file.flush(),
        }
    }
}

#[derive(Default)]
struct GoStyleFields {
    message: Option<String>,
    fields: Vec<(String, String)>,
}

impl Visit for GoStyleFields {
    fn record_debug(&mut self, field: &Field, value: &dyn fmt::Debug) {
        self.record_value(field.name(), format!("{value:?}"));
    }

    fn record_str(&mut self, field: &Field, value: &str) {
        self.record_value(field.name(), value.to_string());
    }

    fn record_bool(&mut self, field: &Field, value: bool) {
        self.record_value(field.name(), value.to_string());
    }

    fn record_i64(&mut self, field: &Field, value: i64) {
        self.record_value(field.name(), value.to_string());
    }

    fn record_u64(&mut self, field: &Field, value: u64) {
        self.record_value(field.name(), value.to_string());
    }

    fn record_error(&mut self, field: &Field, value: &(dyn std::error::Error + 'static)) {
        self.record_value(field.name(), value.to_string());
    }
}

impl GoStyleFields {
    fn record_value(&mut self, name: &str, value: String) {
        if name == "message" {
            self.message = Some(value);
        } else {
            self.fields.push((name.to_string(), value));
        }
    }
}

struct GoStyleFieldFormatter;

impl<'writer> FormatFields<'writer> for GoStyleFieldFormatter {
    fn format_fields<R: tracing_subscriber::field::RecordFields>(
        &self,
        writer: Writer<'writer>,
        fields: R,
    ) -> fmt::Result {
        tracing_subscriber::fmt::format::DefaultFields::new().format_fields(writer, fields)
    }
}

struct GoStyleEventFormatter;

impl<S, N> FormatEvent<S, N> for GoStyleEventFormatter
where
    S: Subscriber + for<'lookup> LookupSpan<'lookup>,
    N: for<'writer> FormatFields<'writer> + 'static,
{
    fn format_event(
        &self,
        _ctx: &FmtContext<'_, S, N>,
        mut writer: Writer<'_>,
        event: &tracing::Event<'_>,
    ) -> fmt::Result {
        let metadata = event.metadata();
        let mut visitor = GoStyleFields::default();
        event.record(&mut visitor);

        let ts = Local::now().format("%Y-%m-%d %H:%M:%S%.3f");
        let level = metadata.level().as_str();
        let file = metadata.file().unwrap_or("unknown");
        let file = file.rsplit('/').next().unwrap_or(file);
        let line = metadata.line().unwrap_or(0);
        let sandbox_id = LOG_SANDBOX_ID
            .get()
            .and_then(|value| value.read().ok().map(|v| v.clone()))
            .unwrap_or_default();
        let message = visitor.message.unwrap_or_default();

        write!(writer, "{ts} [{level}] {file}:{line}")?;
        if !sandbox_id.is_empty() {
            write!(writer, " [{sandbox_id}]")?;
        }
        write!(writer, " {message}")?;
        for (key, value) in visitor.fields {
            write!(writer, " {key}={value}")?;
        }
        writeln!(writer)
    }
}
