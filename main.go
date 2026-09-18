// Command ddc is the Dada Cloud one-button CLI. v0 supports exactly two
// things: `ddc login` (OAuth device authorization grant) and `ddc deploy`
// (package the current directory, upload it, stream the build, print the
// live URL).
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/dada-tuda/ddc/internal/cliapp"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ddc <command>")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  login            sign in via your browser (device code flow)")
	fmt.Fprintln(os.Stderr, "  deploy [dir]     package and deploy dir (default: current directory)")
	fmt.Fprintln(os.Stderr, "  agent <action>   run the agent described by this repo's .dada/agent.json")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "flags for deploy:")
	fmt.Fprintln(os.Stderr, "  --name <name>       app name (default: derived from the directory name)")
	fmt.Fprintln(os.Stderr, "  --project <name>    deploy into an existing project instead of this folder's own")
	fmt.Fprintln(os.Stderr, "  --upload            skip git and upload the folder as an archive")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "agent actions:")
	fmt.Fprintln(os.Stderr, "  spec             show what this repo resolves to")
	fmt.Fprintln(os.Stderr, "  up               start the agent locally from this repo")
	fmt.Fprintln(os.Stderr, "  deploy           ship this repo's agent to the platform")
	fmt.Fprintln(os.Stderr, "  eval             run the eval suites against a local agent")
	fmt.Fprintln(os.Stderr, "  smoke            send one real turn, exit 1 on silence")
	fmt.Fprintln(os.Stderr, "  logs             stream the agent's logs")
	fmt.Fprintln(os.Stderr, "  down             stop the agent")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "flags for agent:")
	fmt.Fprintln(os.Stderr, "  --repo <dir>        agent repo (default: current directory)")
	fmt.Fprintln(os.Stderr, "  --agent <name>      which agent, when the manifest declares several")
	fmt.Fprintln(os.Stderr, "  --prompt <file>     run a different prompt without touching the spec")
	fmt.Fprintln(os.Stderr, "  --mcp <url>         override the tool servers (repeatable)")
	fmt.Fprintln(os.Stderr, "  --port <n>          local A2A port (default: 18081)")
	fmt.Fprintln(os.Stderr, "  --project <name>    deploy target, asked once then remembered")
	fmt.Fprintln(os.Stderr, "  --env <name>        deploy target environment")
	fmt.Fprintln(os.Stderr, "  --suite <name>      eval suite to run (default: every suite)")
	fmt.Fprintln(os.Stderr, "  --dry-run           show what deploy would send, send nothing")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := cliapp.LoadConfig()

	switch os.Args[1] {
	case "login":
		if err := cliapp.Login(ctx, cfg, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "deploy":
		opts, err := parseDeployArgs(os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(2)
		}
		if err := cliapp.Deploy(ctx, cfg, opts, os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "agent":
		if err := runAgent(ctx, cfg, os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func parseDeployArgs(args []string) (cliapp.DeployOptions, error) {
	opts := cliapp.DeployOptions{Dir: "."}
	i := 0
	for i < len(args) {
		switch args[i] {
		case "--name":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--name requires a value")
			}
			opts.AppName = args[i+1]
			i += 2
		case "--project":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--project requires a value")
			}
			opts.Project = args[i+1]
			i += 2
		case "--upload":
			opts.Upload = true
			i++
		default:
			if len(args[i]) > 0 && args[i][0] == '-' {
				return opts, fmt.Errorf("unknown flag %q", args[i])
			}
			opts.Dir = args[i]
			i++
		}
	}
	return opts, nil
}

func agentUsage() {
	fmt.Fprintln(os.Stderr, "usage: ddc agent <spec|up|smoke|eval|deploy|logs|down> [flags]")
}

func runAgent(ctx context.Context, cfg cliapp.Config, args []string, out io.Writer) error {
	if len(args) == 0 {
		agentUsage()
		os.Exit(2)
	}
	action := args[0]
	opts := cliapp.AgentOptions{
		Repo: ".", Port: 18081, Text: "Привет", Tail: "200", Timeout: 180 * time.Second,
	}
	if action == "deploy" {
		opts.Timeout = 10 * time.Minute
	}
	rest := args[1:]
	for i := 0; i < len(rest); {
		needsValue := func() (string, error) {
			if i+1 >= len(rest) {
				return "", fmt.Errorf("%s requires a value", rest[i])
			}
			return rest[i+1], nil
		}
		switch rest[i] {
		case "--repo", "--agent", "--prompt", "--mcp", "--port", "--text", "--tail", "--timeout",
			"--project", "--env", "--suite":
			value, err := needsValue()
			if err != nil {
				return err
			}
			switch rest[i] {
			case "--repo":
				opts.Repo = value
			case "--agent":
				opts.Agent = value
			case "--prompt":
				opts.Prompt = value
			case "--mcp":
				opts.MCP = append(opts.MCP, value)
			case "--port":
				port, err := strconv.Atoi(value)
				if err != nil {
					return fmt.Errorf("--port: %w", err)
				}
				opts.Port = port
			case "--text":
				opts.Text = value
			case "--tail":
				opts.Tail = value
			case "--project":
				opts.Project = value
			case "--env":
				opts.Env = value
			case "--suite":
				opts.Suite = value
			case "--timeout":
				seconds, err := strconv.Atoi(value)
				if err != nil {
					return fmt.Errorf("--timeout: %w", err)
				}
				opts.Timeout = time.Duration(seconds) * time.Second
			}
			i += 2
		case "-f", "--follow":
			opts.Follow = true
			i++
		case "--dry-run":
			opts.DryRun = true
			i++
		default:
			return fmt.Errorf("unknown flag %q", rest[i])
		}
	}

	switch action {
	case "spec":
		return cliapp.AgentSpec(opts, out)
	case "up":
		return cliapp.AgentUp(ctx, cfg, opts, out)
	case "deploy":
		return cliapp.AgentDeploy(ctx, cfg, opts, out)
	case "eval":
		return cliapp.AgentEval(ctx, cfg, opts, out)
	case "smoke":
		return cliapp.AgentSmoke(opts, out)
	case "logs":
		return cliapp.AgentLogs(opts, out)
	case "down":
		return cliapp.AgentDown(opts, out)
	default:
		agentUsage()
		return fmt.Errorf("unknown agent action %q", action)
	}
}
