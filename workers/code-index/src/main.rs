//! Expose the code-index worker's Stage 1 receipt boundary as a stdin/stdout CLI.
//!
//! The binary accepts one restricted JSON request on standard input and emits a
//! canonical local receipt. It intentionally performs no indexing or external
//! effects; later stages replace this local boundary through the canonical
//! contract adapter.

use std::io::{self, Read, Write};

use ouroboros_code_index::{ReceiptError, render_receipt, write_receipt};

const MAX_INPUT_BYTES: u64 = 8_193;

/// Run the Stage 1 local smoke CLI.
///
/// Exit status `0` means one receipt was written. Exit status `2` reports a
/// stable boundary error code on stderr; detailed input is never echoed.
fn main() {
    let mut input = Vec::new();
    let read_result = io::stdin()
        .lock()
        .take(MAX_INPUT_BYTES)
        .read_to_end(&mut input);

    let result = read_result
        .map_err(|_| ReceiptError::InputReadFailed)
        .and_then(|_| render_receipt(&input))
        .and_then(|receipt| write_receipt(&receipt, &mut io::stdout().lock()));

    if let Err(error) = result {
        let _ = writeln!(io::stderr().lock(), "{}", error.code());
        std::process::exit(2);
    }
}
