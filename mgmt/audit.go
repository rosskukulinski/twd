package mgmt

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Audit hash chain (append-only, per chain):
//
//	body    = the record as a JSON object WITHOUT "hash" and "hostSig"
//	          (it includes "prevHash"; "" for the first record of a chain)
//	canon   = body with object keys sorted, no insignificant whitespace,
//	          numbers as written, no HTML escaping
//	hash    = lowercase hex(SHA-256(canon))
//	hostSig = base64(ed25519.Sign(hostKey, []byte(hash)))   (host chains only)
//
// mgmt keeps its own chain ("mgmt"); every host has "host:<hostID>".

// Canonicalize returns the canonical form of a JSON object, dropping the
// named top-level keys.
func Canonicalize(raw []byte, drop ...string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("audit record is not a JSON object")
	}
	for _, k := range drop {
		delete(m, k)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// HashRecord computes the chain hash of a raw record.
func HashRecord(raw []byte) (string, error) {
	c, err := Canonicalize(raw, "hash", "hostSig")
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(c)
	return hex.EncodeToString(s[:]), nil
}

// mgmtRecordBody is the hashed body of a mgmt-chain record.
func mgmtRecordBody(r AuditRecord) ([]byte, error) {
	m := map[string]any{"ts": r.TS, "kind": r.Kind, "prevHash": r.PrevHash}
	put := func(k, v string) {
		if v != "" {
			m[k] = v
		}
	}
	put("tenantID", r.TenantID)
	put("objectID", r.ObjectID)
	put("dst", r.Dst)
	put("grant", r.Grant)
	put("code", r.Code)
	put("traceID", r.TraceID)
	put("actingFor", r.ActingFor)
	if len(r.Detail) > 0 {
		m["detail"] = r.Detail
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return Canonicalize(b)
}

// ChainStatus is the verification result for one chain.
type ChainStatus struct {
	Chain    string `json:"chain"`
	OK       bool   `json:"ok"`
	Records  int    `json:"records"`
	BrokenAt int64  `json:"brokenAt,omitempty"` // seq of the first bad record
	Error    string `json:"error,omitempty"`
}

// VerifyChain recomputes every hash and link of a chain, and checks the
// chain head (detects truncation of the tail). pubKey (optional) verifies
// hostSig on every record.
func VerifyChain(ctx context.Context, s Store, chain string, pubKey ed25519.PublicKey) ChainStatus {
	st := ChainStatus{Chain: chain, OK: true}
	recs, err := s.ListAudit(ctx, AuditFilter{Chain: chain})
	if err != nil {
		return ChainStatus{Chain: chain, Error: err.Error()}
	}
	st.Records = len(recs)
	fail := func(seq int64, f string, a ...any) ChainStatus {
		st.OK, st.BrokenAt, st.Error = false, seq, fmt.Sprintf(f, a...)
		return st
	}
	prev := ""
	var lastSeq int64
	for i, r := range recs {
		if r.Seq != int64(i+1) {
			return fail(r.Seq, "sequence gap: expected %d", i+1)
		}
		h, err := HashRecord(r.Raw)
		if err != nil {
			return fail(r.Seq, "unparseable record: %v", err)
		}
		if h != r.Hash {
			return fail(r.Seq, "hash mismatch (record modified)")
		}
		var link struct {
			PrevHash string `json:"prevHash"`
		}
		json.Unmarshal(r.Raw, &link)
		if link.PrevHash != prev || r.PrevHash != prev {
			return fail(r.Seq, "prevHash does not link to the previous record")
		}
		if pubKey != nil {
			sig, err := base64.StdEncoding.DecodeString(r.HostSig)
			if err != nil || !ed25519.Verify(pubKey, []byte(r.Hash), sig) {
				return fail(r.Seq, "bad hostSig")
			}
		}
		prev, lastSeq = r.Hash, r.Seq
	}
	hseq, hhash, err := s.AuditHead(ctx, chain)
	if err != nil {
		return fail(0, "head: %v", err)
	}
	if hseq != lastSeq || hhash != prev {
		return fail(lastSeq+1, "chain head mismatch (records removed or appended out of band)")
	}
	return st
}
