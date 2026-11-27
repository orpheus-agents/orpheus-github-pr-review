package publication

import (
	"github.com/orpheus-agents/orpheus-github-pr-review/internal/github"
	"github.com/orpheus-agents/orpheus-github-pr-review/internal/protocol"
)

func inlineComment(diff github.Diff, finding protocol.Finding, body string) (github.InlineComment, bool) {
	want := int64(finding.Line)
	for _, file := range diff.Files {
		path, inputPath := file.NewName, file.NewName
		if file.IsDelete {
			path = file.OldName
		}
		if finding.Side == "LEFT" {
			inputPath = file.OldName
		}
		if inputPath != finding.Path {
			continue
		}
		for _, fragment := range file.TextFragments {
			oldLine, newLine := fragment.OldPosition, fragment.NewPosition
			for _, line := range fragment.Lines {
				if finding.Side == "LEFT" && !line.New() && line.Old() && oldLine == want ||
					finding.Side == "RIGHT" && line.New() && newLine == want {
					return github.InlineComment{Path: path, Line: want, Side: finding.Side, Body: body}, true
				}
				if line.Old() {
					oldLine++
				}
				if line.New() {
					newLine++
				}
			}
		}
	}
	return github.InlineComment{}, false
}
