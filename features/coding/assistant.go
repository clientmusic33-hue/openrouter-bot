// Package coding provides programming language detection, coding task
// classification, and specialised engineering prompts.
package coding

import (
	"fmt"
	"strings"
)

// Task identifies the specific software engineering activity requested.
type Task string

const (
	TaskNone           Task = ""
	TaskBugExplanation Task = "bug_explanation"
	TaskErrorAnalysis  Task = "error_analysis"
	TaskRefactoring    Task = "refactoring"
	TaskCodeReview     Task = "code_review"
	TaskTestGeneration Task = "test_generation"
	TaskDockerfile     Task = "dockerfile"
	TaskDeployment     Task = "deployment"
	TaskStackTrace     Task = "stack_trace"
	TaskPatchDiff      Task = "patch_diff"
	TaskGeneralCoding  Task = "general_coding"
)

// Analysis holds the detected language, coding task, and tailored instructions.
type Analysis struct {
	IsCoding   bool
	Language   string
	Task       Task
	Supplement string
}

type langHint struct {
	name    string
	markers []string
}

var langHints = []langHint{
	{"Go", []string{"package main", "func ", "go.mod", "goroutine", "chan ", "fmt.", "err != nil", "golang"}},
	{"Python", []string{"def ", "import ", "traceback (most recent call last)", "pytest", "pip install", "python", "__init__"}},
	{"TypeScript", []string{"interface ", "typescript", ": string", ": number", "tsconfig", "readonly "}},
	{"JavaScript", []string{"const ", "let ", "=>", "console.log", "npm ", "node.js", "javascript"}},
	{"Rust", []string{"fn main()", "let mut ", "cargo ", "impl ", "unwrap()", "rust"}},
	{"Java", []string{"public class ", "public static void main", "nullpointerexception", "spring boot", "maven", "gradle"}},
	{"C/C++", []string{"#include <", "std::", "int main(", "segmentation fault", "valgrind"}},
	{"SQL", []string{"select ", "from ", "where ", "join ", "group by", "create table", "postgres"}},
	{"Dockerfile/Container", []string{"dockerfile", "docker-compose", "from alpine", "entrypoint", "copy --from"}},
	{"Shell/Bash", []string{"#!/bin/bash", "#!/bin/sh", "chmod ", "grep ", "awk ", "sed ", "systemctl"}},
}

// Analyze inspects a prompt to detect whether it is a coding request, which
// programming language it involves, and which engineering task applies.
func Analyze(prompt string) Analysis {
	trimmed := strings.TrimSpace(prompt)
	if trimmed == "" {
		return Analysis{}
	}
	lower := strings.ToLower(trimmed)

	lang := detectLanguage(lower)
	task := detectTask(lower)

	if lang == "" && task == TaskNone {
		return Analysis{}
	}
	if task == TaskNone {
		task = TaskGeneralCoding
	}

	return Analysis{
		IsCoding:   true,
		Language:   lang,
		Task:       task,
		Supplement: buildInstruction(lang, task),
	}
}

func detectLanguage(lower string) string {
	for _, h := range langHints {
		for _, m := range h.markers {
			if strings.Contains(lower, m) {
				return h.name
			}
		}
	}
	return ""
}

func detectTask(lower string) Task {
	switch {
	case strings.Contains(lower, "goroutine") && strings.Contains(lower, "panic:"),
		strings.Contains(lower, "traceback (most recent call last)"),
		strings.Contains(lower, "stack trace"),
		strings.Contains(lower, "at ") && strings.Contains(lower, ".java:"):
		return TaskStackTrace
	case strings.Contains(lower, "dockerfile"),
		strings.Contains(lower, "docker-compose"),
		strings.Contains(lower, "multi-stage build"):
		return TaskDockerfile
	case strings.Contains(lower, "render.yaml"),
		strings.Contains(lower, "deploy"),
		strings.Contains(lower, "kubernetes"),
		strings.Contains(lower, "helm"),
		strings.Contains(lower, "ci/cd"),
		strings.Contains(lower, "github actions"):
		return TaskDeployment
	case strings.Contains(lower, "unit test"),
		strings.Contains(lower, "write test"),
		strings.Contains(lower, "generate test"),
		strings.Contains(lower, "table-driven test"):
		return TaskTestGeneration
	case strings.Contains(lower, "code review"),
		strings.Contains(lower, "review this code"),
		strings.Contains(lower, "audit this"):
		return TaskCodeReview
	case strings.Contains(lower, "refactor"),
		strings.Contains(lower, "clean up this"),
		strings.Contains(lower, "optimize this function"):
		return TaskRefactoring
	case strings.Contains(lower, "diff"),
		strings.Contains(lower, "patch"):
		return TaskPatchDiff
	case strings.Contains(lower, "error:"),
		strings.Contains(lower, "failed with"),
		strings.Contains(lower, "exception"),
		strings.Contains(lower, "compiler error"):
		return TaskErrorAnalysis
	case strings.Contains(lower, "bug"),
		strings.Contains(lower, "why does this fail"),
		strings.Contains(lower, "not working"),
		strings.Contains(lower, "race condition"):
		return TaskBugExplanation
	case strings.Contains(lower, "```"),
		strings.Contains(lower, "write a function"),
		strings.Contains(lower, "implement "):
		return TaskGeneralCoding
	default:
		return TaskNone
	}
}

func buildInstruction(lang string, task Task) string {
	langNote := ""
	if lang != "" {
		langNote = fmt.Sprintf("Detected language/stack: %s. ", lang)
	}

	switch task {
	case TaskStackTrace:
		return langNote + "Coding Assistant Mode (Stack Trace Analysis): Identify the exact throwing frame, explain the root cause of the crash, and show the minimal fix."
	case TaskErrorAnalysis:
		return langNote + "Coding Assistant Mode (Error Analysis): Explain what triggered the error, why it happens, and the exact fix."
	case TaskBugExplanation:
		return langNote + "Coding Assistant Mode (Bug Diagnosis): Pinpoint the logical bug or concurrency hazard, explain how it manifests, and provide corrected code."
	case TaskRefactoring:
		return langNote + "Coding Assistant Mode (Refactoring): Preserve external behaviour while improving readability, performance, and error handling."
	case TaskCodeReview:
		return langNote + "Coding Assistant Mode (Code Review): Check correctness, concurrency safety, security, edge cases, and maintainability."
	case TaskTestGeneration:
		return langNote + "Coding Assistant Mode (Test Generation): Write focused, deterministic unit tests covering happy paths, edge cases, and error paths."
	case TaskDockerfile:
		return langNote + "Coding Assistant Mode (Dockerfile): Use minimal multi-stage builds, non-root runtime user, layer caching, and healthchecks."
	case TaskDeployment:
		return langNote + "Coding Assistant Mode (Deployment Troubleshooting): Check environment variables, ports, healthchecks, permissions, and container lifecycle."
	case TaskPatchDiff:
		return langNote + "Coding Assistant Mode (Patch/Diff): Show a minimal, targeted before/after change without rewriting unrelated code."
	default:
		return langNote + "Coding Assistant Mode: Provide clean, idiomatic, production-ready code with concise explanation."
	}
}
