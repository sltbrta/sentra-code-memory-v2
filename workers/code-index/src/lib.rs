//! Define the deterministic Stage 1 boundary for the code-index worker.
//!
//! This crate deliberately proves only receipt construction and boundary
//! validation. It does not index code, invoke a compiler, read a repository,
//! access a network, or claim compatibility with the canonical contracts that
//! Issue #15 will own.

mod receipt;
mod sha256;

pub use receipt::{ReceiptError, render_receipt, write_receipt};
