package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/qooqapp/qooq/pkg/buildctx"
	"github.com/qooqapp/qooq/pkg/client"
)

func imageCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "image", Short: "Manage images"}

	var sortBy string
	var reverse, showAll, builtOnly bool
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List images available to the daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			images, err := newClient().ListImages(cmd.Context())
			if err != nil {
				return err
			}
			less, err := imageSorter(sortBy)
			if err != nil {
				return err
			}
			// One row per tag, not per image: an image with two tags is two
			// things you can deploy, and collapsing them hides one of the
			// names that works.
			//
			// Untagged (dangling) images are real — an old build whose tag has
			// since moved, still holding disk — but they are unreferenceable by
			// name and there are typically more of them than of everything
			// else, so they are counted rather than listed unless asked for.
			rows := make([]imageRow, 0, len(images))
			dangling := 0
			for _, img := range images {
				if builtOnly && !img.Built {
					continue
				}
				if len(img.Tags) == 0 {
					dangling++
					if showAll {
						rows = append(rows, imageRow{Ref: "<none>", ImageResponse: img})
					}
					continue
				}
				for _, tag := range img.Tags {
					rows = append(rows, imageRow{Ref: tag, ImageResponse: img})
				}
			}
			sort.SliceStable(rows, func(i, j int) bool {
				if reverse {
					return less(&rows[j], &rows[i])
				}
				return less(&rows[i], &rows[j])
			})
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			// BUILT is a column rather than a filter by default: the point is
			// to see at a glance which of these are qooq's doing, on a list
			// that also contains everything else on the machine.
			fmt.Fprintln(w, "REF\tBUILT\tID\tCREATED\tSIZE")
			for _, r := range rows {
				built := "-"
				if r.Built {
					built = "yes"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
					r.Ref, built, shortImageID(r.ID), formatAge(&r.Created), formatBytes(r.Size))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			// Printed rather than left implicit: disk is the usual reason to
			// look at this list at all, and a build machine accumulates these
			// quietly.
			if dangling > 0 && !showAll {
				fmt.Printf("\n%d untagged image(s) hidden; --all to list them\n", dangling)
			}
			return nil
		},
	}
	ls.Flags().StringVar(&sortBy, "sort", "ref", "sort by: ref, created, size")
	ls.Flags().BoolVarP(&reverse, "reverse", "r", false, "reverse the sort order")
	ls.Flags().BoolVarP(&showAll, "all", "a", false, "include untagged (dangling) images")
	ls.Flags().BoolVar(&builtOnly, "built", false, "only images qooq built (the qooq.built label), not ones it pulled")
	cmd.AddCommand(ls)

	var platform string
	pull := &cobra.Command{
		Use:   "pull <ref>",
		Short: "Pull an image by registry ref",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := newClient().PullImage(cmd.Context(), client.PullRequest{Ref: args[0], Platform: platform})
			if err != nil {
				return err
			}
			fmt.Printf("pulled %s -> %s\n", args[0], res.ImageID)
			return nil
		},
	}
	pull.Flags().StringVar(&platform, "platform", "", "target platform, e.g. linux/arm64")
	cmd.AddCommand(pull)

	load := &cobra.Command{
		Use:   "load <tarball>",
		Short: "Load a `docker save` tarball",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := loadTarball(cmd, args[0])
			if err != nil {
				return err
			}
			fmt.Printf("loaded image id(s): %s\n", strings.Join(res.ImageIDs, ", "))
			return nil
		},
	}
	cmd.AddCommand(load)

	var dockerfile, tag string
	build := &cobra.Command{
		Use:   "build <dir>",
		Short: "Build an image from a Dockerfile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := buildFromDir(cmd, args[0], dockerfile, tag)
			if err != nil {
				return err
			}
			fmt.Printf("built image %s\n", res.ImageID)
			return nil
		},
	}
	build.Flags().StringVar(&dockerfile, "dockerfile", "", "Dockerfile path relative to the build context (default: Dockerfile)")
	build.Flags().StringVar(&tag, "tag", "", "image tag, e.g. myapp:latest")
	cmd.AddCommand(build)

	var ignoreMissing bool
	rm := &cobra.Command{
		Use:   "rm <ref>...",
		Short: "Remove images by ref, tag or id",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			// Every ref is attempted even after one fails, and the failures are
			// summarized at the end. Removing images is a cleanup pass over a
			// list: stopping at the first one that is still in use would leave
			// the rest of the list undone for a reason that does not apply to
			// them.
			var failed int
			for _, ref := range args {
				err := c.RemoveImage(cmd.Context(), ref)
				switch {
				case err == nil:
					fmt.Printf("removed %s\n", ref)
				case ignoreMissing && client.IsNotFound(err):
					// Asked for absence, and it is absent.
					fmt.Printf("not present %s\n", ref)
				default:
					fmt.Fprintf(os.Stderr, "error: %s: %v\n", ref, err)
					failed++
				}
			}
			if failed > 0 {
				return fmt.Errorf("%d of %d image(s) could not be removed", failed, len(args))
			}
			return nil
		},
	}
	rm.Flags().BoolVar(&ignoreMissing, "ignore-missing", false, "treat an already-absent image as success (for cleanup scripts)")
	cmd.AddCommand(rm)

	return cmd
}

// imageRow is one printed line: an image paired with the one ref this line is
// about, since an image with several tags occupies several lines.
type imageRow struct {
	Ref string
	client.ImageResponse
}

func imageSorter(field string) (func(a, b *imageRow) bool, error) {
	byRef := func(a, b *imageRow) bool { return a.Ref < b.Ref }
	switch field {
	case "ref", "name":
		return byRef, nil
	case "created":
		// Newest first: an image list is usually read right after a build, to
		// confirm the thing that was just built is there.
		return func(a, b *imageRow) bool {
			if !a.Created.Equal(b.Created) {
				return a.Created.After(b.Created)
			}
			return byRef(a, b)
		}, nil
	case "size":
		return func(a, b *imageRow) bool {
			if a.Size != b.Size {
				return a.Size > b.Size // biggest first: the reason to sort by size
			}
			return byRef(a, b)
		}, nil
	default:
		return nil, fmt.Errorf("unknown --sort field %q: use ref, created or size", field)
	}
}

// shortImageID trims the "sha256:" prefix and the digest to docker's 12
// characters. The full value stays in `image ls`'s JSON source, and any
// unambiguous prefix works wherever a ref is accepted.
func shortImageID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// formatBytes renders a size the way an operator reads it. Powers of 1024, one
// decimal place, matching `docker images`.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGT"[exp])
}

// loadTarball opens path and streams it to POST /v1/images/load; the file is
// never read fully into memory.
func loadTarball(cmd *cobra.Command, path string) (*client.LoadResponse, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return newClient().LoadImage(cmd.Context(), f)
}

// buildFromDir tars dir on the fly (via an io.Pipe, no temp file) and streams
// it to POST /v1/images/build.
func buildFromDir(cmd *cobra.Command, dir, dockerfile, tag string) (*client.BuildResponse, error) {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(buildctx.TarDir(dir, pw))
	}()
	return newClient().BuildImage(cmd.Context(), pr, dockerfile, tag)
}
