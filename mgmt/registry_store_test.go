package mgmt

import (
	"context"
	"slices"
	"testing"

	"github.com/rosskukulinski/twd/internal/registry"
)

// TestStoreRegistryMeta (ADR-032 D3), on SQLite and Postgres: links are
// per repository (a digest linked into one repository is unknown in
// another), tags move, public bases replace by (ref, arch), and deleting a
// tenant drops its namespace's links and tags but no other namespace's.
func TestStoreRegistryMeta(t *testing.T) {
	forEachStore(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		const d1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		const d2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
		if err := s.CreateTenant(ctx, Tenant{ID: "tna", Tailnet: "a.ts.net", MintMode: MintAPIKey, RegistrationsPerMinute: 1, MaxObjects: 1, CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		if err := s.LinkBlob(ctx, "tna/app", d1, 42); err != nil {
			t.Fatal(err)
		}
		if err := s.LinkBlob(ctx, "tna/app", d1, 42); err != nil { // idempotent
			t.Fatal(err)
		}
		if n, ok, err := s.BlobSize(ctx, "tna/app", d1); err != nil || !ok || n != 42 {
			t.Fatalf("blob: %d %v %v", n, ok, err)
		}
		if _, ok, _ := s.BlobSize(ctx, "tnb/app", d1); ok {
			t.Fatal("a link leaked into another namespace")
		}
		if _, ok, _ := s.GetManifest(ctx, "tna/app", d1); ok {
			t.Fatal("a blob link is not a manifest")
		}
		for _, repo := range []string{"tna/app", "public/base"} {
			if err := s.LinkManifest(ctx, repo, registry.ManifestRec{Digest: d2, MediaType: "application/vnd.oci.image.manifest.v1+json", Size: 7}); err != nil {
				t.Fatal(err)
			}
			if err := s.SetTag(ctx, repo, "v1", d1); err != nil {
				t.Fatal(err)
			}
			if err := s.SetTag(ctx, repo, "v1", d2); err != nil { // retag
				t.Fatal(err)
			}
		}
		if m, ok, err := s.GetManifest(ctx, "tna/app", d2); err != nil || !ok || m.Size != 7 || m.MediaType == "" {
			t.Fatalf("manifest %+v %v %v", m, ok, err)
		}
		if d, ok, _ := s.ResolveTag(ctx, "tna/app", "v1"); !ok || d != d2 {
			t.Fatalf("tag: %s %v", d, ok)
		}
		if tags, _ := s.Tags(ctx, "tna/app"); !slices.Equal(tags, []string{"v1"}) {
			t.Fatalf("tags %v", tags)
		}
		p := PublicImage{ID: "pi_1", Ref: "docker.io/x/y:1", Arch: "arm64", Repository: "public/docker.io/x/y", Digest: d1, SourceDigest: d1, CreatedAt: 1}
		if err := s.PutPublicImage(ctx, p); err != nil {
			t.Fatal(err)
		}
		p.ID, p.Digest = "pi_2", d2
		if err := s.PutPublicImage(ctx, p); err != nil { // re-add replaces
			t.Fatal(err)
		}
		if ps, _ := s.ListPublicImages(ctx); len(ps) != 1 || ps[0].ID != "pi_2" || ps[0].Digest != d2 {
			t.Fatalf("public %+v", ps)
		}
		if err := s.DeleteTenant(ctx, "tna"); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.BlobSize(ctx, "tna/app", d1); ok {
			t.Fatal("tenant delete kept its namespace's links")
		}
		if _, ok, _ := s.ResolveTag(ctx, "tna/app", "v1"); ok {
			t.Fatal("tenant delete kept its tags")
		}
		if _, ok, _ := s.GetManifest(ctx, "public/base", d2); !ok {
			t.Fatal("tenant delete dropped public/")
		}
		// "_" in a tenant ID is not a wildcard: deleting tn_a leaves tnxa's.
		if err := s.CreateTenant(ctx, Tenant{ID: "tn_a", Tailnet: "c.ts.net", MintMode: MintAPIKey, RegistrationsPerMinute: 1, MaxObjects: 1, CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
		s.LinkBlob(ctx, "tn_a/app", d1, 1)
		s.LinkBlob(ctx, "tnxa/app", d1, 1)
		if err := s.DeleteTenant(ctx, "tn_a"); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := s.BlobSize(ctx, "tnxa/app", d1); !ok {
			t.Fatal("deleting tenant tn_a dropped namespace tnxa")
		}
		if _, ok, _ := s.BlobSize(ctx, "tn_a/app", d1); ok {
			t.Fatal("tn_a's link survived")
		}
		if err := s.DeletePublicImage(ctx, "pi_2"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeletePublicImage(ctx, "pi_2"); err != ErrNotFound {
			t.Fatalf("second delete: %v", err)
		}
	})
}
