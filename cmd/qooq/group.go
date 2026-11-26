package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/qooqapp/qooq/pkg/client"
)

func groupCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "group", Aliases: []string{"grp"}, Short: "Manage groups"}
	cmd.AddCommand(
		groupCreateCmd(), groupLsCmd(), groupGetCmd(),
		groupAddCmd(), groupRmMemberCmd(),
		groupStartCmd(), groupStopCmd(), groupRmCmd(),
	)
	return cmd
}

func groupCreateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "create <name>",
		Short: "Create a group",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			g, err := newClient().CreateGroup(cmd.Context(), client.CreateGroupRequest{Name: args[0]})
			if err != nil {
				return err
			}
			fmt.Printf("created group %s (%s)\n", g.Name, g.ID)
			return nil
		},
	}
}

func groupLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List groups",
		RunE: func(cmd *cobra.Command, _ []string) error {
			groups, err := newClient().ListGroups(cmd.Context())
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tCREATED")
			for _, g := range groups {
				fmt.Fprintf(w, "%s\t%s\t%s\n", g.ID, g.Name, formatUTC(g.CreatedAt))
			}
			return w.Flush()
		},
	}
}

func groupGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <id-or-name>",
		Short: "Show a group and its members",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := resolveGroup(cmd, args[0], false)
			if err != nil {
				return err
			}
			g, err := newClient().GetGroup(cmd.Context(), ref.ID)
			if err != nil {
				return err
			}
			fmt.Printf("id:      %s\nname:    %s\nmembers: %d\n", g.ID, g.Name, len(g.Members))
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "  NAME\tPHASE\tCONTAINER")
			for _, m := range g.Members {
				fmt.Fprintf(w, "  %s\t%s\t%s\n", m.Name, m.Phase, short(m.ContainerID))
			}
			return w.Flush()
		},
	}
}

func groupAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <group> <component>",
		Short: "Add a component to a group",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			g, err := resolveGroup(cmd, args[0], true)
			if err != nil {
				return err
			}
			c, err := resolve(cmd, args[1])
			if err != nil {
				return err
			}
			out, err := newClient().AddMember(cmd.Context(), g.ID, client.AddMemberRequest{ComponentID: c.ID})
			if err != nil {
				return err
			}
			fmt.Printf("added %s to group %s\n", out.Name, g.Name)
			return nil
		},
	}
}

func groupRmMemberCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm-member <group> <component>",
		Short: "Remove a component from a group",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			g, err := resolveGroup(cmd, args[0], false)
			if err != nil {
				return err
			}
			c, err := resolve(cmd, args[1])
			if err != nil {
				return err
			}
			out, err := newClient().RemoveMember(cmd.Context(), g.ID, c.ID)
			if err != nil {
				return err
			}
			fmt.Printf("removed %s from group %s\n", out.Name, g.Name)
			return nil
		},
	}
}

func groupStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start <id-or-name>",
		Short: "Start all members of a group",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			g, err := resolveGroup(cmd, args[0], false)
			if err != nil {
				return err
			}
			results, err := newClient().StartGroup(cmd.Context(), g.ID)
			if err != nil {
				return err
			}
			printBatch(g.Name, "start", results)
			return nil
		},
	}
}

func groupStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <id-or-name>",
		Short: "Stop all members of a group",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			g, err := resolveGroup(cmd, args[0], false)
			if err != nil {
				return err
			}
			results, err := newClient().StopGroup(cmd.Context(), g.ID)
			if err != nil {
				return err
			}
			printBatch(g.Name, "stop", results)
			return nil
		},
	}
}

func groupRmCmd() *cobra.Command {
	var cascade bool
	cmd := &cobra.Command{
		Use:     "rm <id-or-name>",
		Aliases: []string{"remove"},
		Short:   "Delete a group (--cascade also removes its components)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			g, err := resolveGroup(cmd, args[0], false)
			if err != nil {
				return err
			}
			if err := newClient().DeleteGroup(cmd.Context(), g.ID, cascade); err != nil {
				return err
			}
			fmt.Printf("removed group %s (%s) cascade=%v\n", g.Name, g.ID, cascade)
			return nil
		},
	}
	cmd.Flags().BoolVar(&cascade, "cascade", false, "also remove member components")
	return cmd
}

func printBatch(group, action string, results []client.BatchResultResponse) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "group %s %s:\n", group, action)
	fmt.Fprintln(w, "  NAME\tPHASE\tRESULT")
	for _, r := range results {
		result := "ok"
		if r.Err != "" {
			result = r.Err
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\n", r.Name, r.Phase, result)
	}
	_ = w.Flush()
}

// resolveGroup resolves a group reference (id or name). When create is true and
// no group matches the name, a new group is created (CLI convenience).
func resolveGroup(cmd *cobra.Command, ref string, create bool) (*client.GroupResponse, error) {
	c := newClient()
	if g, err := c.GetGroup(cmd.Context(), ref); err == nil {
		return &g.GroupResponse, nil
	}
	groups, err := c.ListGroups(cmd.Context())
	if err != nil {
		return nil, err
	}
	for i := range groups {
		if groups[i].Name == ref {
			return &groups[i], nil
		}
	}
	if create {
		return c.CreateGroup(cmd.Context(), client.CreateGroupRequest{Name: ref})
	}
	return nil, fmt.Errorf("no group with id or name %q", ref)
}
