// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"
	"github.com/rysh-ai/rysh-cli-code/internal/facetime"
)

const facetimeUsage = `FaceTime audio (local Mac)
  rysh facetime                         Browse contacts with the keyboard
  rysh facetime contacts [--search text] [--offset 0] [--limit 50] [--json]
  rysh facetime call --contact ID --address ID
  rysh facetime doctor
  rysh facetime bot doctor [--json]      Read-only audio/permission inventory
  rysh facetime bot probe --receive UID --transmit UID [--seconds 10] [--json]

Bot probe is an experimental manual audio-routing test, not an operational bot.
It requires two distinct virtual audio devices and native confirmation.

Calls require confirmation in a macOS dialog. FaceTime handles audio and controls.
Contacts JSON output is an explicit local export; do not share it inadvertently.
`

func runFaceTimeCmd(args []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	client := facetime.NativeClient{}
	interactive := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	if len(args) == 0 {
		if !interactive {
			return errors.New("FaceTime picker needs an interactive terminal")
		}
		_, err := tea.NewProgram(facetime.NewPicker(ctx, client), tea.WithAltScreen()).Run()
		return err
	}
	if args[0] == "bridge" {
		return facetime.Bridge(ctx, client, os.Stdin, os.Stdout)
	}
	return faceTimeCommand(ctx, client, args, os.Stdout, interactive)
}
func faceTimeCommand(ctx context.Context, client facetime.Client, args []string, out io.Writer, interactive bool) error {
	if len(args) == 0 {
		return errors.New("missing FaceTime command")
	}
	if args[0] == "bot" {
		return faceTimeBotCommand(ctx, client, args[1:], out, interactive)
	}
	sub := args[0]
	if sub == "help" || sub == "--help" || sub == "-h" {
		fmt.Fprint(out, facetimeUsage)
		return nil
	}
	fs := flag.NewFlagSet("facetime "+sub, flag.ContinueOnError)
	fs.SetOutput(out)
	request := facetime.Request{Version: facetime.Version, Operation: sub}
	jsonOut := false
	switch sub {
	case "contacts":
		fs.StringVar(&request.Search, "search", "", "Search names, phone numbers and emails")
		fs.IntVar(&request.Offset, "offset", 0, "Page offset")
		fs.IntVar(&request.Limit, "limit", 50, "Page size (1–200)")
		fs.BoolVar(&jsonOut, "json", false, "Export this page as JSON")
	case "call":
		fs.StringVar(&request.ContactID, "contact", "", "Contact ID")
		fs.StringVar(&request.AddressID, "address", "", "Address ID")
	case "doctor":
	default:
		return fmt.Errorf("unknown FaceTime command\n%s", facetimeUsage)
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected FaceTime arguments")
	}
	if sub == "contacts" && request.Limit < 1 {
		return errors.New("limit must be between 1 and 200")
	}
	if sub == "call" {
		if !interactive {
			return errors.New("FaceTime calls require an interactive terminal and macOS confirmation")
		}
		if request.ContactID == "" || request.AddressID == "" {
			return errors.New("call requires --contact and --address")
		}
		resolved, err := client.Do(ctx, facetime.Request{Version: facetime.Version, Operation: "resolve", ContactID: request.ContactID})
		if err != nil {
			return err
		}
		if resolved.Contact == nil {
			return errors.New("contact unavailable")
		}
		for _, a := range resolved.Contact.Addresses {
			if a.ID == request.AddressID {
				request.ExpectedValue = a.Value
				break
			}
		}
		if request.ExpectedValue == "" {
			return errors.New("address unavailable; refresh contacts")
		}
	}
	if err := facetime.Validate(request); err != nil {
		return err
	}
	reply, err := client.Do(ctx, request)
	if err != nil {
		return err
	}
	if jsonOut {
		return json.NewEncoder(out).Encode(reply)
	}
	switch sub {
	case "doctor":
		fmt.Fprintf(out, "Contacts permission: %s\nFaceTime audio handler available: %t\nReal audio calls must be verified manually on this Mac.\n", reply.Permission, reply.Available)
		if reply.Handler != "" {
			fmt.Fprintf(out, "Apple audio handler: %s\n", facetime.Display(reply.Handler))
		}
	case "call":
		if reply.Opened {
			fmt.Fprintln(out, "Opened FaceTime audio.")
		} else {
			fmt.Fprintln(out, "Call canceled.")
		}
	case "contacts":
		for _, c := range reply.Contacts {
			fmt.Fprintf(out, "%s  [%s]\n", facetime.Display(c.Name), facetime.Display(c.ID))
			for _, a := range c.Addresses {
				fmt.Fprintf(out, "  %s: %s  [%s]\n", facetime.Display(a.Label), facetime.Display(a.Value), facetime.Display(a.ID))
			}
		}
		if len(reply.Contacts) == 0 {
			fmt.Fprintln(out, "No contacts found.")
		}
		if reply.NextOffset != nil {
			fmt.Fprintf(out, "Next page: --offset %d\n", *reply.NextOffset)
		}
	}
	return nil
}

// FaceTime bot tooling stays local and bypasses daemon configuration/secret loading.
func faceTimeBotCommand(ctx context.Context, client facetime.Client, args []string, out io.Writer, interactive bool) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(out, facetimeUsage)
		return nil
	}
	sub := args[0]
	if sub != "doctor" && sub != "probe" {
		return errors.New("facetime-bot is not implemented yet; use bot doctor or bot probe for the manual feasibility test")
	}
	fs := flag.NewFlagSet("facetime bot "+sub, flag.ContinueOnError)
	fs.SetOutput(out)
	request := facetime.Request{Version: facetime.Version, Operation: "bot_" + sub}
	jsonOut := fs.Bool("json", false, "Print diagnostic JSON (device names and UIDs included)")
	if sub == "probe" {
		fs.StringVar(&request.ReceiveUID, "receive", "", "Virtual device receiving Apple's call output")
		fs.StringVar(&request.TransmitUID, "transmit", "", "Different virtual device selected as Apple's microphone")
		fs.IntVar(&request.Seconds, "seconds", 10, "Probe duration, 5–30 seconds")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected FaceTime bot arguments")
	}
	if sub == "probe" {
		if !interactive {
			return errors.New("audio probe requires an interactive terminal and macOS confirmation")
		}
		if request.Seconds < 5 || request.Seconds > 30 {
			return errors.New("probe duration must be 5–30 seconds")
		}
	}
	if err := facetime.Validate(request); err != nil {
		return err
	}
	reply, err := client.Do(ctx, request)
	if err != nil {
		return err
	}
	if sub == "doctor" && reply.Bot == nil {
		return errors.New("bot diagnostic missing; rebuild Rysh with its matching native helper")
	}
	if sub == "probe" && reply.Probe == nil && reply.Code != "canceled" {
		return errors.New("audio probe returned no measurements")
	}
	if *jsonOut {
		return json.NewEncoder(out).Encode(reply)
	}
	if sub == "doctor" {
		bot := reply.Bot
		fmt.Fprintf(out, "facetime-bot: not implemented; manual feasibility tooling only\nmacOS: %s\nMicrophone permission: %s\nAccessibility trusted: %t\n", facetime.Display(bot.OS), facetime.Display(bot.Microphone), bot.AccessibilityTrusted)
		if reply.Handler != "" {
			fmt.Fprintf(out, "Apple audio handler: %s\n", facetime.Display(reply.Handler))
		}
		for _, device := range bot.Devices {
			fmt.Fprintf(out, "%s: input=%d output=%d virtual=%t\n  UID: %s\n", facetime.Display(device.Name), device.InputChannels, device.OutputChannels, device.Virtual, facetime.Display(device.UID))
		}
		fmt.Fprintln(out, "No audio captured or permissions requested. See docs/facetime-bot.md for the manual routing test.")
	} else if reply.Code == "canceled" {
		fmt.Fprintln(out, "Audio probe canceled.")
	} else {
		probe := reply.Probe
		fmt.Fprintf(out, "Probe: %d seconds, received samples=%d RMS=%.6f peak=%.6f, queued tone frames=%d\n", probe.Seconds, probe.ReceivedSamples, probe.RMS, probe.Peak, probe.QueuedToneFrames)
		fmt.Fprintln(out, "Local measurements only. A human must verify caller speech, return tone and absence of feedback; this is not proof of a working FaceTime bot.")
	}
	return nil
}
