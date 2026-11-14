package mgmt

import (
	"net/http"
	"slices"
	"strings"

	"github.com/rosskukulinski/twd/internal/registry"
	v1 "github.com/rosskukulinski/twd/mgmt/api/v1"
)

// ---- images (ADR-032 D3) ----

func (a *v1api) ListPublicImages(w http.ResponseWriter, r *http.Request, params v1.ListPublicImagesParams) {
	pg, n, err := pageParams(params.Limit, params.Cursor)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	ps, err := a.s.M.ListPublicImages(r.Context())
	if err != nil {
		writeErr(w, r, err)
		return
	}
	slices.SortFunc(ps, func(x, y PublicImage) int {
		if x.CreatedAt != y.CreatedAt {
			if x.CreatedAt < y.CreatedAt {
				return -1
			}
			return 1
		}
		return strings.Compare(x.ID, y.ID)
	})
	if pg.After != nil {
		ps = slices.DeleteFunc(ps, func(p PublicImage) bool {
			return p.CreatedAt < pg.After.Created || p.CreatedAt == pg.After.Created && p.ID <= pg.After.ID
		})
	}
	writeJSON(w, 200, mkPage(ps, n, func(p PublicImage) PageKey { return PageKey{Created: p.CreatedAt, ID: p.ID} }))
}

func (a *v1api) CreatePublicImage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ref    string   `json:"ref"`
		Arches []string `json:"arches"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, r, err)
		return
	}
	if req.Ref == "" {
		writeErr(w, r, errf(400, "bad_request", "ref is required"))
		return
	}
	ps, err := a.s.M.AddPublicImage(r.Context(), req.Ref, req.Arches, principalOf(r.Context()).Actor(r))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]any{"items": ps})
}

func (a *v1api) DeletePublicImage(w http.ResponseWriter, r *http.Request, id string) {
	if err := a.s.M.RemovePublicImage(r.Context(), id, principalOf(r.Context()).Actor(r)); err != nil {
		writeErr(w, r, err)
		return
	}
	w.WriteHeader(204)
}

func (a *v1api) ListImageAliases(w http.ResponseWriter, r *http.Request) {
	al := a.s.M.ImageAliases()
	if al == nil {
		al = []registry.Alias{}
	}
	writeJSON(w, 200, map[string]any{"items": al})
}
