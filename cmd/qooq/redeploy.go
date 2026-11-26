package main

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/qooqapp/qooq/pkg/client"
)

// watchRedeployCmd rebuilds the targets whose components carry an address this
// machine no longer has.
//
// It lives under `watch` rather than `component` because only a rebuild can
// fix this. The stale value was written into the component's environment when
// the watcher resolved ${component.x.endpoint} at build time; re-creating the
// container from the same stored spec would faithfully write the same stale
// value again. A `component redeploy` would look like the fix and not be one,
// which is worse than not offering it.
func watchRedeployCmd() *cobra.Command {
	var (
		staleAddr bool
		dryRun    bool
	)
	cmd := &cobra.Command{
		Use:   "redeploy --stale-addr",
		Short: "Rebuild the targets whose components still name an old address",
		Long: "When this machine changes networks, components keep the address that was\n" +
			"current when they were deployed — their containers run, their health checks\n" +
			"pass, and only a browser finds out. This rebuilds the ones affected, so the\n" +
			"references in their environment resolve again.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !staleAddr {
				return fmt.Errorf("say what to redeploy: --stale-addr")
			}
			comps, err := newClient().ListComponents(cmd.Context())
			if err != nil {
				return err
			}
			var stale []client.ComponentResponse
			for _, c := range comps {
				if c.AdvertiseHostAtDeploy != "" {
					stale = append(stale, c)
				}
			}
			if len(stale) == 0 {
				fmt.Println("nothing to redeploy: no component names an address this machine has lost")
				return nil
			}

			targets, err := newWatcherClient().ListTargets(cmd.Context())
			if err != nil {
				return err
			}
			// A component with no target was deployed by hand, and the address
			// in it was written by a person — a rebuild has nothing to
			// re-resolve. Naming those separately is the whole difference
			// between a report someone can act on and a list they have to
			// work out themselves.
			owned := map[string]string{} // "ns/name" -> target
			for _, t := range targets {
				owned[client.NormalizeNamespace(t.Namespace)+"/"+t.ComponentName] = t.Name
			}

			var toBuild []string
			var byHand []string
			for _, c := range stale {
				key := client.NormalizeNamespace(c.Namespace) + "/" + c.Name
				if name, ok := owned[key]; ok {
					toBuild = append(toBuild, name)
					continue
				}
				byHand = append(byHand, key)
			}
			sort.Strings(toBuild)
			sort.Strings(byHand)

			for _, c := range stale {
				fmt.Printf("  %s: deployed with %s, this machine is now %s\n",
					componentRefName(&c), c.AdvertiseHostAtDeploy, c.AdvertiseHostNow)
			}
			if len(byHand) > 0 {
				fmt.Printf("\nnot rebuildable — deployed by hand, so the address in them was written by you:\n")
				for _, k := range byHand {
					fmt.Printf("  %s\n", k)
				}
				fmt.Printf("  redeploy these yourself with the new address\n")
			}
			if len(toBuild) == 0 {
				return nil
			}

			if dryRun {
				fmt.Printf("\nwould rebuild %d target(s): %v\n", len(toBuild), toBuild)
				return nil
			}
			// Sequential, and each one replaces its container: rebuilding
			// everything at once would take every affected service down
			// together, which is a worse minute than the one being fixed.
			fmt.Printf("\nrebuilding %d target(s) — each will replace its container when its build finishes\n", len(toBuild))
			wc := newWatcherClient()
			for _, name := range toBuild {
				if err := wc.TriggerTarget(cmd.Context(), name, true); err != nil {
					return fmt.Errorf("trigger %s: %w", name, err)
				}
				fmt.Printf("  queued %s\n", name)
			}
			fmt.Printf("\nWatch them with: qooq watch ls\n")
			return nil
		},
	}
	cmd.Flags().BoolVar(&staleAddr, "stale-addr", false, "rebuild every target whose component names an address this machine has lost")
	cmd.Flags().BoolVarP(&dryRun, "dry-run", "n", false, "list what would be rebuilt")
	return cmd
}
