package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nwylynko/clubhouse-cli/controlplane"
	"github.com/nwylynko/clubhouse-cli/placement"
	"github.com/nwylynko/clubhouse-cli/session"
	"github.com/spf13/cobra"
)

func runTake(cmd *cobra.Command, args []string) error {
	s, _, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	services, err := servicesArg(args[0])
	if err != nil {
		return err
	}
	client := controlClient(cmd)
	ctx := context.Background()
	memberID, err := resolveMember(s)
	if err != nil {
		return err
	}
	force, _ := cmd.Flags().GetBool("force")
	hostname, _ := os.Hostname()
	name := strings.TrimPrefix(s.Branch, "client/")
	if name == "" {
		name = hostname
	}

	out := cmd.OutOrStdout()
	var failed []string
	for _, svc := range services {
		lease, err := client.ClaimLease(ctx, s.PIN, controlplane.LeaseRequest{
			Service: svc, MemberID: memberID, Name: name, Machine: hostname,
			TTLSeconds: controlplane.DefaultLeaseTTL, Force: force,
		})
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", svc, err))
			fmt.Fprintf(out, "%-10s refused — %v\n", svc, err)
			continue
		}
		fmt.Fprintf(out, "%-10s taken by %s (%s)\n", svc, lease.OwnerName, lease.Machine)
	}
	if len(failed) > 0 {
		return fmt.Errorf("could not take %s (use --force only for a wedged owner)", strings.Join(failed, "; "))
	}
	fmt.Fprintln(out, "\nthe service starts here on this machine's next tick — leave `club` or `club join` running")
	return nil
}

func runHandOff(cmd *cobra.Command, args []string) error {
	s, _, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	services, err := servicesArg(args[0])
	if err != nil {
		return err
	}
	client := controlClient(cmd)
	ctx := context.Background()
	memberID, err := resolveMember(s)
	if err != nil {
		return err
	}
	to, _ := cmd.Flags().GetString("to")
	toID := ""
	if to != "" {
		if toID, err = memberIDByName(ctx, client, s.PIN, to); err != nil {
			return err
		}
	}

	out := cmd.OutOrStdout()
	for _, svc := range services {
		lease, err := client.HandoverLease(ctx, s.PIN, controlplane.LeaseRequest{
			Service: svc, MemberID: memberID, To: toID, TTLSeconds: controlplane.DefaultLeaseTTL,
		})
		if err != nil {
			return fmt.Errorf("hand off %s: %w", svc, err)
		}
		if lease.Owner == "" {
			fmt.Fprintf(out, "%-10s handed back — the best-ranked member picks it up next tick\n", svc)
			continue
		}
		fmt.Fprintf(out, "%-10s handed to %s (%s)\n", svc, lease.OwnerName, lease.Machine)
	}
	return nil
}

func servicesArg(arg string) ([]string, error) {
	if strings.EqualFold(arg, "all") {
		return controlplane.Services, nil
	}
	for _, s := range controlplane.Services {
		if strings.EqualFold(arg, s) {
			return []string{s}, nil
		}
	}
	return nil, fmt.Errorf("unknown service %q — club places %s", arg, strings.Join(controlplane.Services, ", "))
}

func resolveMember(s session.Session) (string, error) {
	if s.MemberID != "" {
		return s.MemberID, nil
	}
	return "", fmt.Errorf("this machine is not a member of %s — run `club join %s`", s.PIN, s.PIN)
}

func memberIDByName(ctx context.Context, client *controlplane.Client, pin, name string) (string, error) {
	sess, err := client.Session(ctx, pin)
	if err != nil {
		return "", err
	}
	id, err := MemberIDAmong(sess.Members, name)
	if err != nil {
		return "", fmt.Errorf("no member %q in %s — %w", name, pin, err)
	}
	return id, nil
}

func MemberIDAmong(members []controlplane.Member, name string) (string, error) {
	var known []string
	for _, m := range members {
		if m.State == controlplane.MemberLeft {
			continue
		}
		if strings.EqualFold(m.Name, name) || m.ID == name {
			return m.ID, nil
		}
		known = append(known, m.Name)
	}
	return "", fmt.Errorf("members: %s", strings.Join(known, ", "))
}

func runServices(cmd *cobra.Command, _ []string) error {
	s, _, err := sessionCtx(cmd)
	if err != nil {
		return err
	}
	sess, err := controlClient(cmd).Session(context.Background(), s.PIN)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	now := time.Now()
	for _, svc := range controlplane.Services {
		fmt.Fprintf(out, "%-10s %s\n", svc, placement.Describe(sess, svc, now))
	}
	return nil
}
