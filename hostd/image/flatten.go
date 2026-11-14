package image

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// OCI layer whiteouts (image-spec layer.md): ".wh.<name>" deletes <name>
// (and everything under it) from the layers below; ".wh..wh..opq" in a
// directory hides everything the lower layers had in it.
const (
	whiteoutPrefix = ".wh."
	whiteoutOpaque = ".wh..wh..opq"
)

// Layer opens one uncompressed layer tar (Flatten reads each layer twice).
type Layer func() (io.ReadCloser, error)

type ref struct{ layer, idx int }

// cleanName normalises a tar entry name to a relative, slash-separated path
// ("" for the root). Names that climb out of the root are refused.
func cleanName(n string) (string, error) {
	n = strings.TrimPrefix(path.Clean("/"+n), "/")
	if strings.HasPrefix(n, "../") || n == ".." {
		return "", fmt.Errorf("layer entry %q escapes the root", n)
	}
	return n, nil
}

func under(p, dir string) bool { return dir == "" || strings.HasPrefix(p, dir+"/") }

// Flatten applies layers (bottom first) and writes the resulting
// filesystem as one tar to w: whiteouts consumed, overridden entries
// dropped, a non-directory replacing a lower directory hides its subtree.
// Hard links whose target the upper layers removed or replaced become
// regular files with the content the target had when the link was made.
// Entry order is layer order, so every kept hard link follows its target.
// It applies the default Limits (FlattenLimited).
func Flatten(layers []Layer, w io.Writer) error { return FlattenLimited(layers, w, Limits{}) }

// FlattenLimited is Flatten under lim (zero fields: defaults): the first
// pass checks the layer count, every entry (count, name, size) and the
// decompressed bytes as it reads, so nothing is written for an image over
// a limit; the second pass counts the bytes again as it writes. A limit
// error is a *LimitError (errors.Is ErrLimit).
func FlattenLimited(layers []Layer, w io.Writer, lim Limits) error {
	lim = lim.withDefaults()
	if len(layers) > lim.MaxLayers {
		return &LimitError{"layers", int64(lim.MaxLayers)}
	}
	b1 := &budget{lim: lim}
	cur := map[string]ref{}    // path -> the entry that currently provides it
	isDir := map[string]bool{} // path -> current entry is a directory
	linkSrc := map[ref]ref{}   // hard link entry -> the target entry at link time
	for li, open := range layers {
		rc, err := open()
		if err != nil {
			return err
		}
		tr := tar.NewReader(b1.reader(rc))
		for k := 0; ; k++ {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				rc.Close()
				return fmt.Errorf("layer %d: %w", li, err)
			}
			name, err := cleanName(h.Name)
			if err != nil {
				rc.Close()
				return fmt.Errorf("layer %d: %w", li, err)
			}
			if err := b1.entry(name, h.Size); err != nil {
				rc.Close()
				return fmt.Errorf("layer %d: %w", li, err)
			}
			if name == "" {
				continue // the root itself
			}
			dir, base := path.Dir(name), path.Base(name)
			if dir == "." {
				dir = ""
			}
			removeLower := func(p string, self bool) {
				for q, r := range cur {
					if r.layer < li && (self && q == p || under(q, p)) {
						delete(cur, q)
						delete(isDir, q)
					}
				}
			}
			switch {
			case base == whiteoutOpaque:
				removeLower(dir, false)
				continue
			case strings.HasPrefix(base, whiteoutPrefix):
				removeLower(path.Join(dir, strings.TrimPrefix(base, whiteoutPrefix)), true)
				continue
			}
			me := ref{li, k}
			if h.Typeflag == tar.TypeLink {
				tgt, err := cleanName(h.Linkname)
				if err != nil {
					rc.Close()
					return fmt.Errorf("layer %d: %w", li, err)
				}
				src, ok := cur[tgt]
				if !ok {
					rc.Close()
					return fmt.Errorf("layer %d: hard link %s to missing %s", li, name, tgt)
				}
				linkSrc[me] = src
			}
			dirNow := h.Typeflag == tar.TypeDir
			if !dirNow && isDir[name] {
				removeLower(name, false) // a file replaces a lower directory: its subtree goes
			}
			cur[name] = me
			isDir[name] = dirNow
		}
		rc.Close()
	}
	keep := map[ref]string{}
	for p, r := range cur {
		keep[r] = p
	}
	// A kept link whose link-time target is no longer what its target path
	// resolves to (removed or replaced) is materialised from a stash.
	stash := map[ref]bool{}
	materialise := map[ref]ref{}
	linkTo := map[ref]string{}
	for l, src := range linkSrc {
		if _, kept := keep[l]; !kept {
			continue
		}
		// Chains (a link to a link) resolve to the regular file.
		for {
			s2, ok := linkSrc[src]
			if !ok {
				break
			}
			src = s2
		}
		if p, ok := keep[src]; ok {
			linkTo[l] = p
			continue
		}
		stash[src] = true
		materialise[l] = src
	}
	stashed := map[ref]string{}
	defer func() {
		for _, f := range stashed {
			os.Remove(f)
		}
	}()
	stashHdr := map[ref]*tar.Header{}

	b2 := &budget{lim: lim}
	tw := tar.NewWriter(w)
	for li, open := range layers {
		rc, err := open()
		if err != nil {
			return err
		}
		tr := tar.NewReader(b2.reader(rc))
		for k := 0; ; k++ {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				rc.Close()
				return fmt.Errorf("layer %d: %w", li, err)
			}
			me := ref{li, k}
			if stash[me] && h.Typeflag == tar.TypeReg {
				f, err := os.CreateTemp("", "twd-image-link-")
				if err != nil {
					rc.Close()
					return err
				}
				_, err = io.Copy(f, tr)
				f.Close()
				if err != nil {
					rc.Close()
					return err
				}
				stashed[me] = f.Name()
				hc := *h
				stashHdr[me] = &hc
			}
			p, ok := keep[me]
			if !ok {
				continue
			}
			if src, ok := materialise[me]; ok {
				sh, sf := stashHdr[src], stashed[src]
				if sh == nil {
					rc.Close()
					return fmt.Errorf("hard link %s: target content unavailable", p)
				}
				nh := *sh
				nh.Name = p
				if err := writeFile(tw, &nh, sf); err != nil {
					rc.Close()
					return err
				}
				continue
			}
			h.Name = p
			if h.Typeflag == tar.TypeDir {
				h.Name += "/"
			}
			if h.Typeflag == tar.TypeLink {
				h.Linkname = linkTo[me]
			}
			if err := tw.WriteHeader(h); err != nil {
				rc.Close()
				return fmt.Errorf("%s: %w", p, err)
			}
			if h.Typeflag == tar.TypeReg {
				if _, err := io.Copy(tw, tr); err != nil {
					rc.Close()
					return err
				}
			}
		}
		rc.Close()
	}
	return tw.Close()
}

func writeFile(tw *tar.Writer, h *tar.Header, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	h.Typeflag, h.Size, h.Linkname = tar.TypeReg, st.Size(), ""
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}
