pub mod bench;
pub mod codec;
pub mod pipeline;
pub mod protocol;
pub mod server;
pub mod wav;

pub mod log {
    use std::time::{SystemTime, UNIX_EPOCH};

    fn line(level: &str, message: &str) {
        let now = SystemTime::now().duration_since(UNIX_EPOCH).unwrap_or_default();
        eprintln!("{}.{:03} {level} media: {message}", now.as_secs(), now.subsec_millis());
    }

    pub fn info(message: &str) {
        line("INFO", message);
    }

    pub fn warn(message: &str) {
        line("WARN", message);
    }
}
