// Package docs implements the mgit documentation generator.
// Produces 9 agent-facing documentation files per FR-15.
// Refs: FR-15, MGIT-7.1.1
package docs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// DocFile describes a documentation file to generate.
type DocFile struct {
	Name     string
	AutoGen  bool
	Generate func() string
}

// GenerateResult records what happened with each file.
type GenerateResult struct {
	File   string
	Action string // "created", "updated", "skipped"
}

// Generator produces agent-facing documentation files.
// Refs: FR-15, MGIT-7.1.1
type Generator struct {
	outputDir string
	force     bool
	rootCmd   *cobra.Command
	mcpTools  []MCPToolInfo
	version   string
	clock     func() time.Time
}

// MCPToolInfo describes an MCP tool for documentation.
type MCPToolInfo struct {
	Name        string
	Description string
	Parameters  []string
}

// NewGenerator creates a documentation generator.
func NewGenerator(outputDir string, rootCmd *cobra.Command, tools []MCPToolInfo, version string, clock func() time.Time) *Generator {
	return &Generator{
		outputDir: outputDir,
		rootCmd:   rootCmd,
		mcpTools:  tools,
		version:   version,
		clock:     clock,
	}
}

// Generate produces all documentation files.
// Refs: FR-15.1
func (g *Generator) Generate(force bool) ([]GenerateResult, error) {
	g.force = force

	if err := os.MkdirAll(g.outputDir, 0o750); err != nil {
		return nil, fmt.Errorf("create docs dir: %w", err)
	}

	files := g.docFiles()
	var results []GenerateResult

	for _, f := range files {
		result, err := g.generateFile(f)
		if err != nil {
			return results, fmt.Errorf("generate %s: %w", f.Name, err)
		}
		results = append(results, result)
	}

	return results, nil
}

func (g *Generator) generateFile(f DocFile) (GenerateResult, error) {
	path := filepath.Join(g.outputDir, f.Name)

	if !f.AutoGen && !g.force {
		if _, err := os.Stat(path); err == nil {
			return GenerateResult{File: f.Name, Action: "skipped"}, nil
		}
	}

	content := f.Generate()
	action := "created"
	if _, err := os.Stat(path); err == nil {
		action = "updated"
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return GenerateResult{}, fmt.Errorf("write %s: %w", f.Name, err)
	}

	return GenerateResult{File: f.Name, Action: action}, nil
}

func (g *Generator) docFiles() []DocFile {
	return []DocFile{
		{Name: "CLI_REFERENCE.md", AutoGen: true, Generate: g.generateCLIReference},
		{Name: "MCP_TOOLS.md", AutoGen: true, Generate: g.generateMCPTools},
		{Name: "SKILL.md", AutoGen: true, Generate: g.generateSkill},
		{Name: "AGENTS.md", AutoGen: false, Generate: g.generateAgents},
		{Name: "CLAUDE.md", AutoGen: false, Generate: g.generateClaude},
		{Name: "WORKFLOWS.md", AutoGen: false, Generate: g.generateWorkflows},
		{Name: "ROLLBACK_GUIDE.md", AutoGen: false, Generate: g.generateRollbackGuide},
		{Name: "SQUASH_GUIDE.md", AutoGen: false, Generate: g.generateSquashGuide},
		{Name: "TROUBLESHOOTING.md", AutoGen: false, Generate: g.generateTroubleshooting},
	}
}

// generateCLIReference walks the Cobra command tree. Refs: FR-15.8
func (g *Generator) generateCLIReference() string {
	var b strings.Builder
	b.WriteString("# mgit CLI Reference\n\n")
	fmt.Fprintf(&b, "<!-- AUTO-GENERATED: CLI_REFERENCE (v%s) -->\n\n", g.version)

	if g.rootCmd != nil {
		for _, cmd := range g.rootCmd.Commands() {
			if cmd.Hidden {
				continue
			}
			fmt.Fprintf(&b, "## `mgit %s`\n\n", cmd.Name())
			fmt.Fprintf(&b, "%s\n\n", cmd.Short)
			fmt.Fprintf(&b, "```\n%s\n```\n\n", cmd.UseLine())
			if cmd.HasFlags() {
				b.WriteString("**Flags:**\n\n")
				fmt.Fprintf(&b, "```\n%s```\n\n", cmd.Flags().FlagUsages())
			}
		}
	}

	b.WriteString("<!-- END AUTO-GENERATED -->\n")
	return b.String()
}

// generateMCPTools documents all MCP tools. Refs: FR-15.9
func (g *Generator) generateMCPTools() string {
	var b strings.Builder
	b.WriteString("# mgit MCP Tools Reference\n\n")
	b.WriteString("<!-- AUTO-GENERATED: MCP_TOOLS -->\n\n")
	fmt.Fprintf(&b, "**Total tools:** %d\n\n", len(g.mcpTools))

	for _, tool := range g.mcpTools {
		fmt.Fprintf(&b, "## `%s`\n\n", tool.Name)
		fmt.Fprintf(&b, "%s\n\n", tool.Description)
		if len(tool.Parameters) > 0 {
			b.WriteString("**Parameters:**\n")
			for _, p := range tool.Parameters {
				fmt.Fprintf(&b, "- `%s`\n", p)
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("<!-- END AUTO-GENERATED -->\n")
	return b.String()
}

// generateSkill produces SKILL.md with YAML frontmatter. Refs: FR-15.6
func (g *Generator) generateSkill() string {
	toolNames := make([]string, 0, len(g.mcpTools))
	for _, t := range g.mcpTools {
		toolNames = append(toolNames, t.Name)
	}

	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: mgit\n")
	fmt.Fprintf(&b, "version: %s\n", g.version)
	b.WriteString("description: Safety-critical micro version control for LLM agents\n")
	b.WriteString("tools:\n")
	for _, name := range toolNames {
		fmt.Fprintf(&b, "  - %s\n", name)
	}
	b.WriteString("---\n\n")
	b.WriteString("# mgit Skill\n\n")
	b.WriteString("mgit provides task-tagged micro-commits, squash workflows, rollback,\n")
	b.WriteString("and branch management for LLM coding agents.\n")
	return b.String()
}

// --- Template docs ---

func (g *Generator) generateAgents() string {
	return "# AGENTS.md — mgit Agent Quickstart\n\n## What is mgit?\n\nmgit (micro git) is a safety-critical micro version control system for LLM coding agents.\nEvery commit is tagged with a task ID, enabling full traceability and auditability.\n\n## Quick Start\n\n```bash\nmgit init\nmgit commit --task-id=PROJ-1.2.3 --message=\"implement feature\"\nmgit log --task-id=PROJ-1.2.3\nmgit squash --task-id=PROJ-1.2.3\n```\n\n## Available Commands\n\nRun `mgit --help` for the full command list.\nSee CLI_REFERENCE.md for detailed usage.\nSee MCP_TOOLS.md for MCP tool integration.\n"
}

func (g *Generator) generateClaude() string {
	return "# CLAUDE.md — mgit Instructions for Claude Code\n\n## Setup\n\nmgit is available as a CLI tool. Initialize with `mgit init`.\n\n## Workflow\n\n1. `mgit commit --task-id=TASK-ID --message=\"description\"`\n2. `mgit log --task-id=TASK-ID` to verify\n3. `mgit squash --task-id=TASK-ID` when task is complete\n4. `mgit verify` to check integrity\n\n## MCP Tools\n\nmgit exposes 15 MCP tools. See MCP_TOOLS.md for the full reference.\n"
}

func (g *Generator) generateWorkflows() string {
	return "# WORKFLOWS.md — mgit Workflow Guide\n\n## Single Agent Sequential\n\n1. Claim task via mtix\n2. `mgit commit --task-id=TASK-ID` after each change\n3. `mgit squash --task-id=TASK-ID` when done\n4. Mark task done in mtix\n\n## Multi-Agent Parallel\n\n1. Each agent on separate task branch\n2. `mgit branch --task-id=TASK-ID`\n3. Commits isolated per branch\n4. Squash and merge when complete\n\n## Rollback and Rework\n\n1. `mgit rollback --task-id=TASK-ID --dry-run` to preview\n2. `mgit rollback --task-id=TASK-ID` to execute\n3. Original commits preserved (append-only)\n"
}

func (g *Generator) generateRollbackGuide() string {
	return "# ROLLBACK_GUIDE.md\n\n## When to Rollback\n\n- Code review finds issues\n- Requirements changed\n- Need to undo without losing history\n\n## How to Rollback\n\n```bash\nmgit rollback --task-id=TASK-ID --dry-run\nmgit rollback --task-id=TASK-ID --reason=\"review feedback\"\nmgit log --task-id=TASK-ID\n```\n\n## Append-Only\n\nRollback creates a revert commit — never deletes originals.\n"
}

func (g *Generator) generateSquashGuide() string {
	return "# SQUASH_GUIDE.md\n\n## When to Squash\n\n- Task complete, consolidate micro-commits\n- Before merging task branch to main\n\n## How to Squash\n\n```bash\nmgit squash --task-id=TASK-ID --dry-run\nmgit squash --task-id=TASK-ID\nmgit log --task-id=TASK-ID\n```\n\n## What Happens\n\n- Micro-commits consolidated into single squash commit\n- Original commits remain (append-only)\n"
}

func (g *Generator) generateTroubleshooting() string {
	return "# TROUBLESHOOTING.md\n\n## Common Issues\n\n### \"repository already exists\"\nRun `mgit init` only once. Use `mgit status` to check.\n\n### \"task not found\"\nCheck task ID format (PREFIX-N.N.N). Verify with `mgit log --task-id=ID`.\n\n### \"commit not found\"\nUse full 40-character hash. Check with `mgit log`.\n\n### \"branch locked\"\nWait for timeout (30s) or check with `mgit branch`.\n\n### \"append-only constraint violated\"\nUse `mgit rollback` to create revert commits.\n\n## Verify Integrity\n\n```bash\nmgit verify\nmgit verify --task-id=ID\n```\n"
}
