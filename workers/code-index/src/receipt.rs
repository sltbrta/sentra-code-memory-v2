//! Parse the Stage 1 local request and render its canonical smoke receipt.
//!
//! This module intentionally supports a restricted ASCII JSON object with
//! `input` and `config` string fields only. Raw backslashes and therefore all
//! JSON escape forms are rejected before parsing, preventing an implied
//! general-purpose protocol before the canonical contract is frozen.

use std::collections::BTreeMap;
use std::fmt;
use std::io::Write;

use crate::sha256::sha256_hex;

const MAX_REQUEST_BYTES: usize = 8_192;
const MAX_FIELD_BYTES: usize = 4_096;
const SCHEMA_VERSION: &str = "stage1.local.smoke.v1";
const OPERATION: &str = "stage1-local-smoke";
const RUNTIME: &str = "stage1-local-worker";

/// Represent a stable, non-sensitive error at the Stage 1 worker boundary.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ReceiptError {
    /// Report that reading standard input failed before a receipt could exist.
    InputReadFailed,
    /// Report a request larger than the bounded Stage 1 local contract permits.
    InputTooLarge,
    /// Report malformed or top-level non-object restricted JSON.
    InvalidJson,
    /// Report a valid JSON shape that violates the restricted request contract.
    InvalidRequest,
    /// Report a failed receipt write without leaking an operating-system error.
    OutputWriteFailed,
}

impl ReceiptError {
    /// Return the stable machine-readable code for this error.
    #[must_use]
    pub const fn code(&self) -> &'static str {
        match self {
            Self::InputReadFailed => "OURO-STAGE1-INPUT-READ-FAILED",
            Self::InputTooLarge => "OURO-STAGE1-INPUT-TOO-LARGE",
            Self::InvalidJson => "OURO-STAGE1-INVALID-JSON",
            Self::InvalidRequest => "OURO-STAGE1-INVALID-REQUEST",
            Self::OutputWriteFailed => "OURO-STAGE1-OUTPUT-WRITE-FAILED",
        }
    }
}

impl fmt::Display for ReceiptError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.code())
    }
}

impl std::error::Error for ReceiptError {}

/// Render a deterministic Stage 1 local smoke receipt from one bounded request.
///
/// The request must be a UTF-8 JSON object with exactly two non-empty ASCII
/// string fields: `input` and `config`. Raw backslashes, JSON escapes, control
/// characters, and non-ASCII string bytes are outside this local grammar.
/// Values are represented only by SHA-256 digests, so the resulting receipt
/// never exposes the original payload.
///
/// # Errors
///
/// Returns [`ReceiptError::InputTooLarge`] when `request` exceeds 8 KiB;
/// [`ReceiptError::InvalidJson`] for malformed or top-level non-object JSON; and
/// [`ReceiptError::InvalidRequest`] for missing, duplicate, extra, empty, or
/// non-string fields, raw backslashes, escapes, or non-ASCII strings.
///
/// # Examples
///
/// ```
/// use ouroboros_code_index::render_receipt;
///
/// let receipt = render_receipt(br#"{"input":"evidence","config":"stage1-config"}"#)?;
/// assert!(receipt.contains("stage1.local.smoke.v1"));
/// # Ok::<(), ouroboros_code_index::ReceiptError>(())
/// ```
pub fn render_receipt(request: &[u8]) -> Result<String, ReceiptError> {
    if request.len() > MAX_REQUEST_BYTES {
        return Err(ReceiptError::InputTooLarge);
    }
    if request.contains(&b'\\') {
        return Err(ReceiptError::InvalidRequest);
    }
    let source = std::str::from_utf8(request).map_err(|_| ReceiptError::InvalidJson)?;
    let fields = RestrictedJson::new(source).parse_object()?;
    let input = required_field(&fields, "input")?;
    let config = required_field(&fields, "config")?;

    if fields.len() != 2 || input.is_empty() || config.is_empty() {
        return Err(ReceiptError::InvalidRequest);
    }
    if input.len() > MAX_FIELD_BYTES || config.len() > MAX_FIELD_BYTES {
        return Err(ReceiptError::InvalidRequest);
    }

    Ok(format!(
        "{{\"config_digest\":\"sha256:{}\",\"input_digest\":\"sha256:{}\",\"operation\":\"{}\",\"runtime\":\"{}\",\"schema_version\":\"{}\",\"status\":\"ok\"}}\n",
        sha256_hex(config.as_bytes()),
        sha256_hex(input.as_bytes()),
        OPERATION,
        RUNTIME,
        SCHEMA_VERSION,
    ))
}

/// Write one complete canonical receipt to an already-selected output stream.
///
/// # Errors
///
/// Returns [`ReceiptError::OutputWriteFailed`] when the stream rejects the
/// write. The underlying operating-system error is intentionally not exposed.
pub fn write_receipt<W: Write>(receipt: &str, writer: &mut W) -> Result<(), ReceiptError> {
    writer
        .write_all(receipt.as_bytes())
        .and_then(|()| writer.flush())
        .map_err(|_| ReceiptError::OutputWriteFailed)
}

fn required_field<'a>(
    fields: &'a BTreeMap<String, String>,
    name: &str,
) -> Result<&'a str, ReceiptError> {
    fields
        .get(name)
        .map(String::as_str)
        .ok_or(ReceiptError::InvalidRequest)
}

struct RestrictedJson<'a> {
    source: &'a [u8],
    cursor: usize,
}

impl<'a> RestrictedJson<'a> {
    fn new(source: &'a str) -> Self {
        Self {
            source: source.as_bytes(),
            cursor: 0,
        }
    }

    fn parse_object(mut self) -> Result<BTreeMap<String, String>, ReceiptError> {
        self.skip_whitespace();
        self.expect_byte(b'{')?;
        self.skip_whitespace();
        let mut fields = BTreeMap::new();
        if self.consume_byte(b'}') {
            self.ensure_finished()?;
            return Ok(fields);
        }

        loop {
            self.skip_whitespace();
            let key = self.parse_ascii_string()?;
            self.skip_whitespace();
            self.expect_byte(b':')?;
            self.skip_whitespace();
            if self.current_byte() != Some(b'\"') {
                return Err(ReceiptError::InvalidRequest);
            }
            let value = self.parse_ascii_string()?;
            if fields.insert(key, value).is_some() {
                return Err(ReceiptError::InvalidRequest);
            }
            self.skip_whitespace();
            if self.consume_byte(b'}') {
                self.ensure_finished()?;
                return Ok(fields);
            }
            self.expect_byte(b',')?;
        }
    }

    fn parse_ascii_string(&mut self) -> Result<String, ReceiptError> {
        self.expect_byte(b'\"')?;
        let start = self.cursor;
        while let Some(byte) = self.current_byte() {
            match byte {
                b'\"' => {
                    let value = std::str::from_utf8(&self.source[start..self.cursor])
                        .map_err(|_| ReceiptError::InvalidJson)?;
                    self.cursor += 1;
                    return Ok(value.to_owned());
                }
                0..=31 => return Err(ReceiptError::InvalidJson),
                b'\\' | 127..=u8::MAX => return Err(ReceiptError::InvalidRequest),
                _ => self.cursor += 1,
            }
        }
        Err(ReceiptError::InvalidJson)
    }

    fn expect_byte(&mut self, expected: u8) -> Result<(), ReceiptError> {
        if self.consume_byte(expected) {
            Ok(())
        } else {
            Err(ReceiptError::InvalidJson)
        }
    }

    fn consume_byte(&mut self, expected: u8) -> bool {
        if self.current_byte() == Some(expected) {
            self.cursor += 1;
            true
        } else {
            false
        }
    }

    fn current_byte(&self) -> Option<u8> {
        self.source.get(self.cursor).copied()
    }

    fn skip_whitespace(&mut self) {
        while matches!(self.current_byte(), Some(b' ' | b'\n' | b'\r' | b'\t')) {
            self.cursor += 1;
        }
    }

    fn ensure_finished(&mut self) -> Result<(), ReceiptError> {
        self.skip_whitespace();
        if self.cursor == self.source.len() {
            Ok(())
        } else {
            Err(ReceiptError::InvalidJson)
        }
    }
}

#[cfg(test)]
mod tests {
    use std::io::{self, Write};

    use super::{ReceiptError, render_receipt, write_receipt};

    const MATRIX: &str = include_str!("../tests/receipt_matrix.tsv");
    const EXPECTED: &str = include_str!("../tests/expected_receipt.json");

    #[test]
    fn matches_every_shared_matrix_outcome() -> Result<(), Box<dyn std::error::Error>> {
        for matrix_case in matrix_cases()? {
            let actual = match render_receipt(&matrix_case.request) {
                Ok(receipt) if receipt == EXPECTED => "RECEIPT",
                Ok(_) => "NON_CANONICAL_RECEIPT",
                Err(error) => error.code(),
            };
            assert_eq!(actual, matrix_case.expected, "{}", matrix_case.name);
        }
        Ok(())
    }

    #[test]
    fn rejects_size_boundaries() {
        let oversized = format!("{{\"input\":\"{}\",\"config\":\"x\"}}", "a".repeat(4_097));
        assert_eq!(
            render_receipt(oversized.as_bytes()),
            Err(ReceiptError::InvalidRequest)
        );
        assert_eq!(
            render_receipt(&vec![b'x'; 8_193]),
            Err(ReceiptError::InputTooLarge)
        );
    }

    #[test]
    fn is_deterministic_for_duplicate_invocations() -> Result<(), Box<dyn std::error::Error>> {
        let normal = matrix_cases()?
            .into_iter()
            .find(|matrix_case| matrix_case.name == "normal")
            .ok_or_else(|| io::Error::other("normal receipt matrix case is missing"))?;
        assert_eq!(
            render_receipt(&normal.request),
            render_receipt(&normal.request)
        );
        Ok(())
    }

    #[test]
    fn reports_output_failure_without_leaking_io_detail() {
        struct FailingWriter;

        impl Write for FailingWriter {
            fn write(&mut self, _: &[u8]) -> io::Result<usize> {
                Err(io::Error::other("private output detail"))
            }

            fn flush(&mut self) -> io::Result<()> {
                Ok(())
            }
        }

        assert_eq!(
            write_receipt(EXPECTED, &mut FailingWriter),
            Err(ReceiptError::OutputWriteFailed)
        );
    }

    struct MatrixCase<'a> {
        name: &'a str,
        request: Vec<u8>,
        expected: &'a str,
    }

    fn matrix_cases() -> Result<Vec<MatrixCase<'static>>, Box<dyn std::error::Error>> {
        let mut cases = Vec::new();
        for (line_number, line) in MATRIX.lines().enumerate() {
            if line.is_empty() || line.starts_with('#') {
                continue;
            }
            let columns: Vec<&str> = line.split('\t').collect();
            if columns.len() != 3 {
                return Err(io::Error::other(format!(
                    "receipt matrix line {} must contain three columns",
                    line_number + 1
                ))
                .into());
            }
            cases.push(MatrixCase {
                name: columns[0],
                request: decode_hex(columns[1])?,
                expected: columns[2],
            });
        }
        Ok(cases)
    }

    fn decode_hex(encoded: &str) -> Result<Vec<u8>, Box<dyn std::error::Error>> {
        if !encoded.len().is_multiple_of(2) {
            return Err(io::Error::other("receipt matrix hex must have even length").into());
        }
        (0..encoded.len())
            .step_by(2)
            .map(|index| u8::from_str_radix(&encoded[index..index + 2], 16).map_err(Into::into))
            .collect()
    }
}
