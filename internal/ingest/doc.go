// Package ingest will implement the signed agent ingest path (DESIGN.md §9):
// Ed25519 request signatures over the canonical string
//
//	smokeng-ingest-v1\n<METHOD>\n<PATH>\n<agent_id>\n<timestamp>\n<nonce>\n<hex(sha256(body))>
//
// with the validation order: agent enabled → timestamp window (±300s) →
// nonce unseen (in-memory, TTL 600s) → signature → target assignment. The
// idempotent measurement upsert in the store is the real replay defense.
//
// Status: not implemented before v0.4; the wire format above is frozen by the
// agreed design.
package ingest
