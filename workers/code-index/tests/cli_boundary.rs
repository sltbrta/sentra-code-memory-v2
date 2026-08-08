//! Exercise the Stage 1 code-index CLI process boundary without ambient state.

use std::fs;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::{SystemTime, UNIX_EPOCH};

const FIXTURE: &str = r#"{"input":"evidence","config":"stage1-config"}"#;

#[test]
fn runs_from_an_unrelated_directory_with_hostile_environment()
-> Result<(), Box<dyn std::error::Error>> {
    let directory = TestDirectory::new()?;

    let mut child = Command::new(env!("CARGO_BIN_EXE_ouroboros-code-index"))
        .current_dir(directory.path())
        .env_clear()
        .env("HOME", directory.path().join("isolated-home"))
        .env("PATH", "")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()?;
    let mut stdin = child
        .stdin
        .take()
        .ok_or_else(|| std::io::Error::other("worker stdin was unavailable"))?;
    stdin.write_all(FIXTURE.as_bytes())?;
    drop(stdin);

    let output = child.wait_with_output()?;
    assert!(output.status.success());
    assert_eq!(
        String::from_utf8(output.stdout)?,
        concat!(
            "{\"config_digest\":\"sha256:89e6d294343e1fb818ae00328cd0bc472318f28348390ce520807449681df24a\",",
            "\"input_digest\":\"sha256:ee8250fb76e094b34b471f13a73dbbe51d1ae142e9df59d7c0d31ec20f0a0a8e\",",
            "\"operation\":\"stage1-local-smoke\",",
            "\"runtime\":\"stage1-local-worker\",",
            "\"schema_version\":\"stage1.local.smoke.v1\",",
            "\"status\":\"ok\"}\n"
        )
    );
    assert!(output.stderr.is_empty());
    Ok(())
}

#[cfg(unix)]
#[test]
fn exits_without_recursion_when_standard_error_is_closed() -> Result<(), Box<dyn std::error::Error>>
{
    let directory = TestDirectory::new()?;
    let mut child = Command::new("/bin/sh")
        .args([
            "-c",
            "exec 2>&-; exec \"$1\"",
            "ouroboros-closed-stderr",
            env!("CARGO_BIN_EXE_ouroboros-code-index"),
        ])
        .current_dir(directory.path())
        .env_clear()
        .env("HOME", directory.path().join("isolated-home"))
        .env("PATH", "")
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()?;
    let mut stdin = child
        .stdin
        .take()
        .ok_or_else(|| std::io::Error::other("worker stdin was unavailable"))?;
    stdin.write_all(b"[]")?;
    drop(stdin);

    let output = child.wait_with_output()?;
    assert_eq!(output.status.code(), Some(2));
    assert!(output.stdout.is_empty());
    assert!(output.stderr.is_empty());
    Ok(())
}

/// Regression test for issue #140: concurrent `TestDirectory::new` calls from
/// threaded tests collided on a pid-plus-timestamp name.
#[test]
fn concurrent_test_directories_never_collide() -> Result<(), Box<dyn std::error::Error>> {
    let handles: Vec<_> = (0..16)
        .map(|_| std::thread::spawn(TestDirectory::new))
        .collect();
    let mut directories = Vec::new();
    for handle in handles {
        directories.push(
            handle
                .join()
                .map_err(|_| std::io::Error::other("test directory thread panicked"))??,
        );
    }

    let unique: std::collections::HashSet<&Path> =
        directories.iter().map(TestDirectory::path).collect();
    assert_eq!(unique.len(), directories.len());
    for directory in &directories {
        assert!(directory.path().is_dir());
    }
    Ok(())
}

struct TestDirectory(PathBuf);

impl TestDirectory {
    fn new() -> std::io::Result<Self> {
        // Threaded tests share a process id and can observe identical
        // timestamps, so a process-wide counter is what makes each name
        // unique within the test binary; pid and timestamp keep names
        // unique across processes and stale leftovers.
        static NEXT_DIRECTORY_ID: AtomicU64 = AtomicU64::new(0);
        let unique = NEXT_DIRECTORY_ID.fetch_add(1, Ordering::Relaxed);
        let timestamp = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_err(|_| std::io::Error::other("system time preceded the Unix epoch"))?
            .as_nanos();
        let path = std::env::temp_dir().join(format!(
            "ouroboros-code-index-{}-{timestamp}-{unique}",
            std::process::id()
        ));
        fs::create_dir(&path)?;
        Ok(Self(path))
    }

    fn path(&self) -> &Path {
        &self.0
    }
}

impl Drop for TestDirectory {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}
