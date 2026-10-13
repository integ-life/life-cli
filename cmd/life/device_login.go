package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/flyfy1/life-cli/internal/integlife"
)

func cmdLogin(ctx context.Context, client *integlife.Client, args []string) error {
	fs := flag.NewFlagSet("life login", flag.ContinueOnError)
	password := fs.Bool("password", false, "use interactive username/password login")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: life login [--password]")
	}
	if *password {
		return cmdPasswordLogin(ctx, client)
	}
	hostname, _ := os.Hostname()
	name := "life-cli"
	if hostname != "" {
		name += " on " + hostname
	}
	code, err := client.RequestDeviceCode(ctx, name)
	if err != nil {
		return fmt.Errorf("start login: %w", err)
	}
	fmt.Printf("Open %s\nEnter code: %s\n", code.VerificationURI, code.UserCode)
	target := code.VerificationURI
	if code.VerificationURIComplete != "" {
		fmt.Printf("Or open: %s\n", code.VerificationURIComplete)
		target = code.VerificationURIComplete
	}
	fmt.Printf("Waiting for browser approval (expires in %d minutes)…\n", code.ExpiresIn/60)
	tryOpenBrowser(ctx, target)
	token, err := client.PollDeviceToken(ctx, code)
	if err != nil {
		return err
	}
	if err := integlife.SaveAPIToken(token); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	fmt.Println("login ok: token saved")
	if os.Getenv("INTEGLIFE_API_TOKEN") != "" {
		fmt.Println("INTEGLIFE_API_TOKEN is set and continues to override the saved token.")
	}
	return nil
}

func browserCommand(target string) []string {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return nil
	}
	switch runtime.GOOS {
	case "darwin":
		return []string{"open", target}
	case "windows":
		return []string{"rundll32", "url.dll,FileProtocolHandler", target}
	case "linux":
		if os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "" {
			return []string{"xdg-open", target}
		}
	}
	return nil
}

func tryOpenBrowser(ctx context.Context, target string) {
	command := browserCommand(target)
	if len(command) == 0 {
		return
	}
	browserCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = exec.CommandContext(browserCtx, command[0], command[1:]...).Run()
}

func cmdAuthStatus(ctx context.Context, client *integlife.Client, args []string) error {
	if len(args) != 1 || args[0] != "status" {
		return errors.New("usage: life auth status")
	}
	loggedIn, err := client.AuthStatus(ctx)
	if err != nil {
		return err
	}
	if !loggedIn {
		fmt.Println("not logged in; run life login")
		return errors.New("no valid login")
	}
	source := "saved token"
	if os.Getenv("INTEGLIFE_API_TOKEN") != "" {
		source = "INTEGLIFE_API_TOKEN"
	}
	fmt.Printf("logged in (%s)\n", source)
	return nil
}
