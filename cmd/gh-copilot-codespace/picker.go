package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
	"github.com/ekroon/gh-copilot-codespace/internal/workspace"
)

var errWorkspaceSelectionCancelled = errors.New("workspace selection cancelled")

var interactiveTerminalFunc = isInteractiveTerminal
var workspacePickerFunc = selectWorkspaceSessionTUI

type workspacePickerModel struct {
	input     textinput.Model
	all       []workspace.WorkspaceSummary
	filtered  []workspace.WorkspaceSummary
	cursor    int
	selected  string
	cancelled bool
}

func newWorkspacePickerModel(list []workspace.WorkspaceSummary) workspacePickerModel {
	input := textinput.New()
	input.Prompt = "Search: "
	input.Placeholder = "type to filter repositories, codespaces, branches, paths..."
	input.CharLimit = 0
	input.Focus()
	input.SetValue("")

	model := workspacePickerModel{
		input: input,
		all:   append([]workspace.WorkspaceSummary(nil), list...),
	}
	model.applyFilter()
	return model
}

func (m workspacePickerModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m workspacePickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			m.cancelled = true
			return m, tea.Quit
		case tea.KeyEnter:
			if len(m.filtered) == 0 {
				return m, nil
			}
			m.selected = m.filtered[m.cursor].Name
			return m, tea.Quit
		case tea.KeyUp:
			if m.cursor > 0 {
				m.cursor--
			}
			return m, nil
		case tea.KeyDown:
			if m.cursor < len(m.filtered)-1 {
				m.cursor++
			}
			return m, nil
		}

		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		m.applyFilter()
		return m, cmd
	}

	return m, nil
}

func (m workspacePickerModel) View() string {
	var b strings.Builder
	b.WriteString("Choose workspace session to resume\n\n")
	b.WriteString(m.input.View())
	b.WriteString("\n\n")

	if len(m.filtered) == 0 {
		b.WriteString("  No matching workspace sessions.\n")
	} else {
		for i, ws := range m.filtered {
			cursor := "  "
			if i == m.cursor {
				cursor = "> "
			}
			b.WriteString(cursor)
			b.WriteString(workspaceSummaryDetails(ws))
			b.WriteByte('\n')
		}
	}

	b.WriteString("\nEnter selects • Esc/Ctrl-C cancels\n")
	return b.String()
}

func (m *workspacePickerModel) setQuery(query string) {
	m.input.SetValue(query)
	m.applyFilter()
}

func (m *workspacePickerModel) applyFilter() {
	m.filtered = filterWorkspaceSummaries(m.all, m.input.Value())
	if len(m.filtered) == 0 {
		m.cursor = 0
		return
	}
	if m.cursor >= len(m.filtered) {
		m.cursor = len(m.filtered) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func selectWorkspaceSessionTUI(list []workspace.WorkspaceSummary, input, output *os.File) (string, error) {
	model := newWorkspacePickerModel(list)
	program := tea.NewProgram(model, tea.WithInput(input), tea.WithOutput(output))
	finalModel, err := program.Run()
	if err != nil {
		return "", err
	}

	finalPicker, ok := finalModel.(workspacePickerModel)
	if !ok {
		return "", fmt.Errorf("unexpected workspace picker model type %T", finalModel)
	}
	if finalPicker.cancelled {
		return "", errWorkspaceSelectionCancelled
	}
	if finalPicker.selected == "" {
		return "", fmt.Errorf("no workspace session selected")
	}
	return finalPicker.selected, nil
}

func isInteractiveTerminal() bool {
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stdout.Fd())
}
