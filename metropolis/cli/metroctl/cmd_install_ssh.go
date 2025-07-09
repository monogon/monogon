// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/schollz/progressbar/v3"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/term"
	"google.golang.org/protobuf/proto"

	"source.monogon.dev/osbase/net/sshtakeover"
	"source.monogon.dev/osbase/oci"
)

// progressbarUpdater wraps a [progressbar.ProgressBar] with an improved
// interface for updating progress. It updates the progress bar in a separate
// goroutine and at most 60 times per second. The stop function stops the
// updates and can be safely called multiple times.
type progressbarUpdater struct {
	bar    *progressbar.ProgressBar
	update chan int64
	close  chan struct{}
}

func startProgressbarUpdater(bar *progressbar.ProgressBar) *progressbarUpdater {
	updater := &progressbarUpdater{
		bar:    bar,
		update: make(chan int64, 1),
		close:  make(chan struct{}),
	}
	go updater.run()
	return updater
}

func (p *progressbarUpdater) add(num int64) {
	for {
		select {
		case p.update <- num:
			return
		case oldNum := <-p.update:
			num += oldNum
		}
	}
}

func (p *progressbarUpdater) run() {
	for {
		select {
		case num := <-p.update:
			p.bar.Add64(num)
		case <-p.close:
			return
		}
		select {
		case <-time.After(time.Second / 60):
		case <-p.close:
			return
		}
	}
}

func (p *progressbarUpdater) stop() {
	if p.close == nil {
		return
	}
	p.close <- struct{}{}
	p.close = nil
	select {
	case num := <-p.update:
		// Do one last update to make the bar reach 100%.
		p.bar.Add64(num)
	default:
	}
	if !p.bar.IsFinished() {
		p.bar.Exit()
	}
}

var sshCmd = &cobra.Command{
	Use:     "ssh --disk=<disk> <target>",
	Short:   "Installs Metropolis on a Linux system accessible via SSH.",
	Example: "metroctl install --image=metropolis-v0.1 --takeover=takeover ssh --disk=nvme0n1 root@ssh-enabled-server.example",
	Args:    cobra.ExactArgs(1), // One positional argument: the target
	RunE: func(cmd *cobra.Command, args []string) error {
		user, address, err := parseSSHAddr(args[0])
		if err != nil {
			return err
		}

		diskName, err := cmd.Flags().GetString("disk")
		if err != nil {
			return err
		}

		if len(diskName) == 0 {
			return fmt.Errorf("flag disk is required")
		}

		var authMethods []ssh.AuthMethod
		if aconn, err := net.Dial("unix", os.Getenv("SSH_AUTH_SOCK")); err == nil {
			defer aconn.Close()
			a := agent.NewClient(aconn)
			authMethods = append(authMethods, ssh.PublicKeysCallback(a.Signers))
		} else {
			log.Printf("error while establishing ssh agent connection: %v", err)
			log.Println("ssh agent authentication will not be available.")
		}

		// On Windows syscall.Stdin is a handle and needs to be cast to an
		// int for term.
		stdin := int(syscall.Stdin) // nolint:unconvert
		if term.IsTerminal(stdin) {
			authMethods = append(authMethods,
				ssh.PasswordCallback(func() (string, error) {
					fmt.Printf("%s@%s's password: ", user, address)
					b, err := term.ReadPassword(stdin)
					if err != nil {
						return "", err
					}
					fmt.Println()
					return string(b), nil
				}),
				ssh.KeyboardInteractive(func(name, instruction string, questions []string, echos []bool) ([]string, error) {
					answers := make([]string, 0, len(questions))
					for i, q := range questions {
						fmt.Print(q)
						if echos[i] {
							if _, err := fmt.Scan(&questions[i]); err != nil {
								return nil, err
							}
						} else {
							b, err := term.ReadPassword(stdin)
							if err != nil {
								return nil, err
							}
							fmt.Println()
							answers = append(answers, string(b))
						}
					}
					return answers, nil
				}),
			)
		} else {
			log.Println("stdin is not interactive. password authentication will not be available.")
		}

		conf := &ssh.ClientConfig{
			User: user,
			Auth: authMethods,
			// Ignore the host key, since it's likely the first time anything logs into
			// this device, and also because there's no way of knowing its fingerprint.
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			// Timeout sets a bound on the time it takes to set up the connection, but
			// not on total session time.
			Timeout: 5 * time.Second,
		}

		ctx, _ := signal.NotifyContext(context.Background(), os.Interrupt)
		conn, err := sshtakeover.Dial(ctx, address, conf)
		if err != nil {
			return fmt.Errorf("error while establishing ssh connection: %w", err)
		}

		params, err := makeNodeParams()
		if err != nil {
			return err
		}
		rawParams, err := proto.Marshal(params)
		if err != nil {
			return fmt.Errorf("error while marshaling node params: %w", err)
		}

		const takeoverTargetPath = "/root/takeover"
		const imageTargetPath = "/root/osimage"

		imagePathResolved, err := external("image", "_main/metropolis/node/oci_image", imagePath)
		if err != nil {
			return err
		}
		image, err := oci.ReadLayout(imagePathResolved)
		if err != nil {
			return fmt.Errorf("failed to read OS image: %w", err)
		}
		imageLayout, err := oci.CreateLayout(image)
		if err != nil {
			return fmt.Errorf("failed to read OS image: %w", err)
		}
		takeoverPath, err := cmd.Flags().GetString("takeover")
		if err != nil {
			return err
		}
		takeover, err := externalFile("takeover", "_main/metropolis/cli/takeover/takeover_/takeover", &takeoverPath)
		if err != nil {
			return err
		}

		log.Println("Uploading files to target host.")
		totalSize := takeover.Size()
		for _, entry := range imageLayout.Walk() {
			if entry.Mode.IsRegular() {
				totalSize += entry.Content.Size()
			}
		}
		barUpdater := startProgressbarUpdater(progressbar.DefaultBytes(totalSize))
		defer barUpdater.stop()
		conn.SetProgress(barUpdater.add)

		takeoverContent, err := takeover.Open()
		if err != nil {
			return err
		}
		err = conn.UploadExecutable(ctx, takeoverTargetPath, takeoverContent)
		takeoverContent.Close()
		if err != nil {
			return fmt.Errorf("error while uploading %q: %w", takeoverTargetPath, err)
		}

		err = conn.UploadTree(ctx, imageTargetPath, imageLayout)
		if err != nil {
			return fmt.Errorf("error while uploading OS image: %w", err)
		}

		barUpdater.stop()

		// Start the agent and wait for the agent's output to arrive.
		log.Printf("Starting the takeover executable at path %q.", takeoverTargetPath)
		_, stderr, err := conn.Execute(ctx, fmt.Sprintf("%s -disk %s", takeoverTargetPath, diskName), rawParams)
		stderrStr := strings.TrimSpace(string(stderr))
		if stderrStr != "" {
			log.Printf("Agent stderr: %q", stderrStr)
		}
		if err != nil {
			return fmt.Errorf("while starting the takeover executable: %w", err)
		}

		return nil
	},
}

func parseAddrOptionalPort(addr string) (string, string, error) {
	if addr == "" {
		return "", "", fmt.Errorf("address is empty")
	}

	idx := strings.LastIndex(addr, ":")
	// IPv4, DNS without Port.
	if idx == -1 {
		return addr, "", nil
	}

	// IPv4, DNS with Port.
	if strings.Count(addr, ":") == 1 {
		return addr[:idx], addr[idx+1:], nil
	}

	// IPv6 with Port.
	if addrPort, err := netip.ParseAddrPort(addr); err == nil {
		return addrPort.Addr().String(), fmt.Sprintf("%d", addrPort.Port()), nil
	}

	// IPv6 without Port.
	if addr, err := netip.ParseAddr(addr); err == nil {
		return addr.String(), "", nil
	}

	return "", "", fmt.Errorf("failed to parse address: %q", addr)
}

func parseSSHAddr(s string) (string, string, error) {
	user, rawAddr, ok := strings.Cut(s, "@")
	if !ok {
		return "", "", fmt.Errorf("SSH user is mandatory")
	}

	addr, port, err := parseAddrOptionalPort(rawAddr)
	if err != nil {
		return "", "", err
	}
	if port == "" {
		port = "22"
	}

	return user, net.JoinHostPort(addr, port), nil
}

func init() {
	sshCmd.Flags().String("disk", "", "Which disk Metropolis should be installed to")
	sshCmd.Flags().String("takeover", "", "Path to the Metropolis takeover binary")

	installCmd.AddCommand(sshCmd)
}
