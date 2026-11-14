package mgmt

import (
	"context"
	"database/sql"
	"errors"

	"github.com/rosskukulinski/twd/internal/registry"
)

// ---- OCI registry metadata (ADR-032 D3) ----
//
// registry_links says which repository may see which content: a blob
// (kind "blob") or a manifest (kind "manifest", with its media type). The
// bytes are content-addressed files under Config.RegistryDir; Postgres and
// SQLite hold only this metadata.

func (s *SQLStore) link(ctx context.Context, repo, digest, kind, mt string, size int64) error {
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO registry_links (repo, digest, kind, media_type, size, created_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT (repo, digest, kind) DO NOTHING`), repo, digest, kind, mt, size, s.Now().UnixMilli())
	return err
}

// LinkBlob implements registry.Meta.
func (s *SQLStore) LinkBlob(ctx context.Context, repo, digest string, size int64) error {
	return s.link(ctx, repo, digest, "blob", "", size)
}

// BlobSize implements registry.Meta.
func (s *SQLStore) BlobSize(ctx context.Context, repo, digest string) (int64, bool, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, s.q("SELECT size FROM registry_links WHERE repo=? AND digest=? AND kind='blob'"), repo, digest).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return n, err == nil, err
}

// LinkManifest implements registry.Meta.
func (s *SQLStore) LinkManifest(ctx context.Context, repo string, m registry.ManifestRec) error {
	return s.link(ctx, repo, m.Digest, "manifest", m.MediaType, m.Size)
}

// GetManifest implements registry.Meta.
func (s *SQLStore) GetManifest(ctx context.Context, repo, digest string) (registry.ManifestRec, bool, error) {
	m := registry.ManifestRec{Digest: digest}
	err := s.db.QueryRowContext(ctx, s.q("SELECT media_type, size FROM registry_links WHERE repo=? AND digest=? AND kind='manifest'"), repo, digest).Scan(&m.MediaType, &m.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return m, false, nil
	}
	return m, err == nil, err
}

// SetTag implements registry.Meta.
func (s *SQLStore) SetTag(ctx context.Context, repo, tag, digest string) error {
	_, err := s.db.ExecContext(ctx, s.q(`INSERT INTO registry_tags (repo, tag, digest, updated_at) VALUES (?,?,?,?)
		ON CONFLICT (repo, tag) DO UPDATE SET digest=excluded.digest, updated_at=excluded.updated_at`), repo, tag, digest, s.Now().UnixMilli())
	return err
}

// ResolveTag implements registry.Meta.
func (s *SQLStore) ResolveTag(ctx context.Context, repo, tag string) (string, bool, error) {
	var d string
	err := s.db.QueryRowContext(ctx, s.q("SELECT digest FROM registry_tags WHERE repo=? AND tag=?"), repo, tag).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return d, err == nil, err
}

// Tags implements registry.Meta.
func (s *SQLStore) Tags(ctx context.Context, repo string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.q("SELECT tag FROM registry_tags WHERE repo=? ORDER BY tag"), repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- public base images ----

const publicCols = "id, ref, arch, repo, digest, source_digest, created_by, created_at"

// PutPublicImage inserts p, replacing the entry for the same (ref, arch).
func (s *SQLStore) PutPublicImage(ctx context.Context, p PublicImage) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.q("DELETE FROM public_images WHERE ref=? AND arch=?"), p.Ref, p.Arch); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, s.q("INSERT INTO public_images ("+publicCols+") VALUES (?,?,?,?,?,?,?,?)"),
			p.ID, p.Ref, p.Arch, p.Repository, p.Digest, p.SourceDigest, p.CreatedBy, p.CreatedAt)
		return err
	})
}

// ListPublicImages lists every public base image (ordered by ref, arch).
func (s *SQLStore) ListPublicImages(ctx context.Context) ([]PublicImage, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+publicCols+" FROM public_images ORDER BY ref, arch")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PublicImage
	for rows.Next() {
		var p PublicImage
		if err := rows.Scan(&p.ID, &p.Ref, &p.Arch, &p.Repository, &p.Digest, &p.SourceDigest, &p.CreatedBy, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePublicImage removes one entry (ErrNotFound if absent). The
// registry content stays (running containers keep their image).
func (s *SQLStore) DeletePublicImage(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, s.q("DELETE FROM public_images WHERE id=?"), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
