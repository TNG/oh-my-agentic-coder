package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpprobe"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
	"github.com/TNG/oh-my-agentic-coder/internal/secrets"
)

const toolProbeTimeout = 30 * time.Second

// Review status of one tool relative to the last review.
const (
	reviewStatusReviewed = "reviewed"
	reviewStatusChanged  = "changed"
	reviewStatusNew      = "new"
	reviewStatusRemoved  = "removed"
)

type setFlag []string

func (s *setFlag) String() string     { return strings.Join(*s, ",") }
func (s *setFlag) Set(v string) error { *s = append(*s, v); return nil }

func runMCPTools(args []string, env *Env) int {
	fs := flag.NewFlagSet("mcp tools", flag.ContinueOnError)
	scopeFlag := fs.String("scope", scopeProject, "Where the server is registered: project (this workdir) or global.")
	classify := fs.Bool("classify", false, "Record a class for every tool and record the list as reviewed.")
	var sets setFlag
	fs.Var(&sets, "set", "With --classify: NAME=CLASS (read, write, delete, off); repeatable. Skips the prompts.")
	rest := fs.String("rest", "", "With --classify: class for every tool not named by --set (default off). Skips the prompts.")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: omac mcp tools <name> [--scope project|global] [--classify [--set NAME=CLASS ...] [--rest CLASS]]")
		fs.PrintDefaults()
	}
	if code, ok := parseFlags(fs, args, env); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return ExitMisuse
	}
	name := fs.Arg(0)
	if (len(sets) > 0 || *rest != "") && !*classify {
		fmt.Fprintln(env.Stderr, "omac mcp tools: --set and --rest need --classify")
		return ExitMisuse
	}
	if refuseInsideSandbox(env, "tools") {
		return ExitMisuse
	}
	scope, err := resolveMCPScope(*scopeFlag, env)
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac mcp tools:", err)
		return ExitMisuse
	}
	var picked map[string]mcppolicy.Class
	if *classify {
		if picked, err = parseClassifyFlags(sets, *rest); err != nil {
			fmt.Fprintln(env.Stderr, "omac mcp tools:", err)
			return ExitMisuse
		}
	}
	entry, live, ok := probeMCPServer(env, "tools", scope, name)
	if !ok {
		return ExitGeneric
	}
	printToolTable(env, entry, live)
	if !*classify {
		return ExitOK
	}
	return classifyMCPTools(env, scope, entry, live, picked, *rest, len(sets) > 0 || *rest != "")
}

// probeMCPServer loads the registration and lists the live tools of its
// server. On a start failure it prints the stderr tail and the caller must
// not touch the registry.
func probeMCPServer(env *Env, cmd string, scope mcpreg.Scope, name string) (*mcpreg.Entry, []mcppolicy.ToolInfo, bool) {
	entry, err := mcpreg.Load(scope, name)
	if err != nil {
		fmt.Fprintf(env.Stderr, "omac mcp %s: %v\n", cmd, err)
		return nil, nil, false
	}
	secs := map[string]secrets.Secret{}
	defer func() {
		for _, s := range secs {
			s.Zero()
		}
	}()
	for _, sname := range entry.Def.Secrets {
		v, err := mcpreg.GetSecret(scope, name, sname)
		if err != nil {
			fmt.Fprintf(env.Stderr, "omac mcp %s: secret %s: %v; register the server again to store it\n", cmd, sname, err)
			return nil, nil, false
		}
		secs[sname] = v
	}
	live, tail, err := mcpprobe.ListTools(context.Background(), entry, secs, probeBaseEnv(entry.Def), toolProbeTimeout)
	if err != nil {
		fmt.Fprintf(env.Stderr, "omac mcp %s: server %s failed: %s\n", cmd, name, stripControlChars(err.Error()))
		if strings.TrimSpace(tail) != "" {
			fmt.Fprintf(env.Stderr, "server stderr (last 2 KiB):\n%s\n", stripControlChars(strings.TrimRight(tail, "\n")))
		}
		return nil, nil, false
	}
	return entry, live, true
}

// probeBaseEnv is what the gateway hands its children: the base passthrough
// variables and the ones the definition names, taken from the host.
func probeBaseEnv(def *mcpreg.Definition) []string {
	names := append(config.DefaultLauncherConfig().Facade.BaseEnvPassthrough, def.Server.EnvPassthrough...)
	var out []string
	for _, n := range names {
		if v, ok := os.LookupEnv(n); ok {
			out = append(out, n+"="+v)
		}
	}
	return out
}

func liveHashes(live []mcppolicy.ToolInfo) map[string]string {
	h := make(map[string]string, len(live))
	for _, t := range live {
		h[t.Name] = t.Hash
	}
	return h
}

func currentClasses(def *mcpreg.Definition) map[string]mcppolicy.Class {
	m := make(map[string]mcppolicy.Class, len(def.Tools))
	for _, t := range def.Tools {
		m[t.Name] = t.Class
	}
	return m
}

func sortedLive(live []mcppolicy.ToolInfo) []mcppolicy.ToolInfo {
	out := append([]mcppolicy.ToolInfo(nil), live...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func toolReviewStatus(review *mcppolicy.Review, t mcppolicy.ToolInfo) string {
	if review == nil {
		return reviewStatusNew
	}
	reviewed, ok := review.Tools[t.Name]
	switch {
	case !ok:
		return reviewStatusNew
	case reviewed != t.Hash:
		return reviewStatusChanged
	}
	return reviewStatusReviewed
}

func printToolTable(env *Env, entry *mcpreg.Entry, live []mcppolicy.ToolInfo) {
	review := entry.Def.Review
	current := currentClasses(entry.Def)
	fmt.Fprintf(env.Stdout, "Tools of %s. Proposed classes come from the server's own annotations: they are claims, not facts, so check each tool yourself.\n\n", stripControlChars(entry.Name))
	tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TOOL\tPROPOSED\tREASON\tCURRENT\tREVIEW")
	unclassified := 0
	liveNames := map[string]bool{}
	for _, t := range sortedLive(live) {
		liveNames[t.Name] = true
		class, reason := mcppolicy.Propose(t)
		cur := "unclassified"
		if c, ok := current[t.Name]; ok {
			cur = string(c)
		} else {
			unclassified++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", stripControlChars(t.Name), class, stripControlChars(reason), cur, toolReviewStatus(review, t))
	}
	var removed []string
	if review != nil {
		for n := range review.Tools {
			if !liveNames[n] {
				removed = append(removed, n)
			}
		}
		sort.Strings(removed)
	}
	for _, n := range removed {
		cur := "unclassified"
		if c, ok := current[n]; ok {
			cur = string(c)
		}
		fmt.Fprintf(tw, "%s\t-\t-\t%s\t%s\n", stripControlChars(n), cur, reviewStatusRemoved)
	}
	tw.Flush()
	fmt.Fprintf(env.Stdout, "\n%s\n", toolSummary(review, live, unclassified, len(removed)))
}

func toolSummary(review *mcppolicy.Review, live []mcppolicy.ToolInfo, unclassified, removed int) string {
	s := fmt.Sprintf("%d tools, %d unclassified; ", len(live), unclassified)
	if review == nil {
		return s + "not reviewed"
	}
	d := mcppolicy.Compare(review, live)
	if d.Empty() {
		return s + "reviewed " + review.At.Format("2006-01-02") + ", no changes"
	}
	return s + fmt.Sprintf("reviewed %s, since then %d new, %d changed, %d removed", review.At.Format("2006-01-02"), len(d.Added), len(d.Changed), removed)
}

func parseClass(s string) (mcppolicy.Class, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "r", "read":
		return mcppolicy.Read, true
	case "w", "write":
		return mcppolicy.Write, true
	case "d", "delete":
		return mcppolicy.Delete, true
	case "off":
		return mcppolicy.Off, true
	}
	return "", false
}

func parseClassifyFlags(sets []string, rest string) (map[string]mcppolicy.Class, error) {
	picked := map[string]mcppolicy.Class{}
	for _, s := range sets {
		n, c, ok := strings.Cut(s, "=")
		class, valid := parseClass(c)
		if !ok || n == "" || !valid {
			return nil, fmt.Errorf("--set %q: want NAME=CLASS with CLASS one of read, write, delete, off", s)
		}
		if _, dup := picked[n]; dup {
			return nil, fmt.Errorf("--set names %q twice", n)
		}
		picked[n] = class
	}
	if rest != "" {
		if _, ok := parseClass(rest); !ok {
			return nil, fmt.Errorf("--rest %q: want read, write, delete or off", rest)
		}
	}
	return picked, nil
}

func classifyMCPTools(env *Env, scope mcpreg.Scope, entry *mcpreg.Entry, live []mcppolicy.ToolInfo, picked map[string]mcppolicy.Class, rest string, scripted bool) int {
	var classes map[string]mcppolicy.Class
	var err error
	if scripted {
		classes, err = scriptedClasses(live, picked, rest)
	} else {
		classes, err = promptClasses(env, live)
	}
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac mcp tools:", err)
		return ExitMisuse
	}
	hashes := liveHashes(live)
	_, err = mcpreg.Update(scope, entry.Name, func(d *mcpreg.Definition) error {
		d.Tools = d.Tools[:0]
		for _, t := range sortedLive(live) {
			d.Tools = append(d.Tools, mcppolicy.Tool{Name: t.Name, Class: classes[t.Name]})
		}
		d.Review = &mcppolicy.Review{ToolsHash: mcppolicy.ListHash(hashes), Tools: hashes, At: time.Now().UTC()}
		return nil
	})
	if err != nil {
		fmt.Fprintln(env.Stderr, "omac mcp tools:", err)
		return ExitIOError
	}
	fmt.Fprintf(env.Stdout, "\nclassified and recorded as reviewed %d tools of %s\n", len(live), stripControlChars(entry.Name))
	for _, t := range sortedLive(live) {
		fmt.Fprintf(env.Stdout, "  %s  %s\n", classes[t.Name], stripControlChars(t.Name))
	}
	notifyMCPReload(env, scope)
	return ExitOK
}

func scriptedClasses(live []mcppolicy.ToolInfo, picked map[string]mcppolicy.Class, rest string) (map[string]mcppolicy.Class, error) {
	restClass := mcppolicy.Off
	if rest != "" {
		restClass, _ = parseClass(rest)
	}
	liveNames := map[string]bool{}
	classes := map[string]mcppolicy.Class{}
	for _, t := range live {
		liveNames[t.Name] = true
		if c, ok := picked[t.Name]; ok {
			classes[t.Name] = c
		} else {
			classes[t.Name] = restClass
		}
	}
	for n := range picked {
		if !liveNames[n] {
			return nil, fmt.Errorf("--set names %q, which the server does not list", n)
		}
	}
	return classes, nil
}

func promptClasses(env *Env, live []mcppolicy.ToolInfo) (map[string]mcppolicy.Class, error) {
	in := bufio.NewReader(env.Stdin)
	classes := map[string]mcppolicy.Class{}
	fmt.Fprintln(env.Stdout, "\nClassify each tool: r(ead), w(rite), d(elete) or off. Enter takes the proposal.")
	for _, t := range sortedLive(live) {
		proposal, reason := mcppolicy.Propose(t)
		for {
			fmt.Fprintf(env.Stdout, "  %s [proposed %s: %s] (r/w/d/off) [%s]: ", stripControlChars(t.Name), proposal, stripControlChars(reason), proposal)
			line, err := in.ReadString('\n')
			if err != nil && strings.TrimSpace(line) == "" {
				return nil, errors.New("input ended before every tool was classified; nothing was written (use --set/--rest to classify without prompts)")
			}
			if strings.TrimSpace(line) == "" {
				classes[t.Name] = proposal
				break
			}
			if c, ok := parseClass(line); ok {
				classes[t.Name] = c
				break
			}
			fmt.Fprintln(env.Stdout, "  answer r, w, d or off")
		}
	}
	return classes, nil
}

// notifyMCPReload tells a running omac session of this workdir to pick up the
// change. It is best effort: a failure is only a warning.
func notifyMCPReload(env *Env, scope mcpreg.Scope) {
	report := func(ok bool, msg string) {
		if ok {
			fmt.Fprintln(env.Stdout, msg)
		} else if msg != "" && msg != "no running omac serve detected" {
			fmt.Fprintf(env.Stderr, "warning: %s\n", msg)
		}
	}
	report(notifyReload(env.Workdir))
	if scope.Key == mcpreg.GlobalScopeKey {
		report(notifyReloadGlobal())
	}
}
