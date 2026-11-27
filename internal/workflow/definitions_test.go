package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const testFrontMatter = "---\nid: review\nprofile: review-profile\nsandbox_template: review-sandbox\nservices: [github]\n"

func writeWorkflow(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
}

func TestLoadWorkflowsRoutesRepositoriesAndSnapshotsInstructions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fallback := testFrontMatter + "---\n\nFallback rules.\n"
	special := strings.Replace(testFrontMatter, "id: review", "id: special", 1) + "repositories: [team/api, Team/web, other-team/api]\nmodel: custom-model\nlanguage: ru\n---\n\nSpecial rules.\n"
	writeWorkflow(t, dir, "a-default.md", fallback)
	writeWorkflow(t, dir, "z-special.md", special)
	writeWorkflow(t, dir, ".temporary.md", "invalid")
	writeWorkflow(t, dir, "README.txt", "invalid")
	require.NoError(t, os.Mkdir(filepath.Join(dir, "nested.md"), 0o700))
	workflows, err := Load(dir, 4096)
	require.NoError(t, err)
	for _, name := range []string{"team/api", "team/web", "Other-Team/API"} {
		selected, ok := workflows.Select(name)
		require.True(t, ok)
		require.Equal(t, "special", selected.ID)
		require.Equal(t, "\nSpecial rules.\n", selected.Instructions)
		require.Equal(t, "ru", selected.Language)
		require.Equal(t, "custom-model", selected.Model)
		selected.Services[0] = "mutated"
		selected.Repositories[0] = "other/value"
	}
	for _, name := range []string{"team/another", "other-team/web", "third-team/api"} {
		selected, ok := workflows.Select(name)
		require.True(t, ok)
		require.Equal(t, "review", selected.ID)
		require.Empty(t, selected.Language)
	}
	writeWorkflow(t, dir, "z-special.md", "invalid changed file")
	selected, ok := workflows.Select("team/api")
	require.True(t, ok)
	require.Equal(t, []string{"github"}, selected.Services)
	require.Equal(t, []string{"team/api", "Team/web", "other-team/api"}, selected.Repositories)
	require.Equal(t, "\nSpecial rules.\n", selected.Instructions)
	_, err = Load(dir, 4096)
	require.ErrorContains(t, err, "z-special.md")
}

func TestLoadWorkflowsWithoutFallbackAndWithEmptyServices(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := strings.Replace(testFrontMatter, "[github]", "[]", 1) + "repositories: [team/api]\n---\r\nInstructions\r\n"
	writeWorkflow(t, dir, "review.md", content)
	workflows, err := Load(dir, 4096)
	require.NoError(t, err)
	selected, ok := workflows.Select("team/api")
	require.True(t, ok)
	require.NotNil(t, selected.Services)
	require.Empty(t, selected.Services)
	require.Equal(t, "Instructions\r\n", selected.Instructions)
	_, ok = workflows.Select("team/web")
	require.False(t, ok)
	_, ok = (Set{}).Select("team/api")
	require.False(t, ok)
}

func TestLoadWorkflowsRejectsInvalidDefinitionsAtomically(t *testing.T) {
	t.Parallel()
	valid := testFrontMatter + "---\nInstructions\n"
	cases := map[string]string{
		"empty file":             "",
		"boolean service":        strings.Replace(valid, "[github]", "[true]", 1),
		"numeric repository":     testFrontMatter + "repositories: [1]\n---\nInstructions",
		"missing front matter":   "Instructions",
		"unclosed":               testFrontMatter + "Instructions",
		"unknown key":            testFrontMatter + "timeout: 10\n---\nInstructions",
		"duplicate key":          testFrontMatter + "id: another\n---\nInstructions",
		"missing id":             strings.Replace(valid, "id: review\n", "", 1),
		"invalid id":             strings.Replace(valid, "id: review", "id: ../review", 1),
		"missing profile":        strings.Replace(valid, "profile: review-profile\n", "", 1),
		"missing template":       strings.Replace(valid, "sandbox_template: review-sandbox\n", "", 1),
		"missing services":       strings.Replace(valid, "services: [github]\n", "", 1),
		"null services":          strings.Replace(valid, "[github]", "null", 1),
		"duplicate services":     strings.Replace(valid, "[github]", "[github, github]", 1),
		"invalid service":        strings.Replace(valid, "[github]", "[GitHub]", 1),
		"selector scalar":        testFrontMatter + "repositories: team/api\n---\nInstructions",
		"empty selector":         testFrontMatter + "repositories: []\n---\nInstructions",
		"null selector":          testFrontMatter + "repositories: null\n---\nInstructions",
		"duplicate repositories": testFrontMatter + "repositories: [team/api, Team/API]\n---\nInstructions",
		"unqualified repository": testFrontMatter + "repositories: [api]\n---\nInstructions",
		"invalid repository":     testFrontMatter + "repositories: [team/..]\n---\nInstructions",
		"invalid language":       testFrontMatter + "language: Russian please\n---\nInstructions",
		"non-string profile":     strings.Replace(valid, "profile: review-profile", "profile: 10", 1),
		"empty body":             testFrontMatter + "---\n \n",
		"invalid UTF8":           valid + string([]byte{0xff}),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeWorkflow(t, dir, "a-valid.md", strings.Replace(valid, "id: review", "id: valid", 1))
			writeWorkflow(t, dir, "bad.md", content)
			workflows, err := Load(dir, 4096)
			require.ErrorContains(t, err, "bad.md")
			_, ok := workflows.Select("team/api")
			require.False(t, ok, "partial configuration must not escape on failure")
		})
	}
}

func TestLoadWorkflowsRejectsAmbiguousRouting(t *testing.T) {
	t.Parallel()
	cases := map[string][2]string{
		"duplicate id":      {testFrontMatter + "repositories: [team/api]\n", testFrontMatter + "repositories: [team/web]\n"},
		"multiple fallback": {testFrontMatter, strings.Replace(testFrontMatter, "id: review", "id: other", 1)},
		"overlap":           {testFrontMatter + "repositories: [team/api, team/web]\n", strings.Replace(testFrontMatter, "id: review", "id: other", 1) + "repositories: [Team/Web, other-team/api]\n"},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for i, prefix := range files {
				writeWorkflow(t, dir, []string{"a.md", "b.md"}[i], prefix+"---\nInstructions")
			}
			_, err := Load(dir, 4096)
			require.ErrorContains(t, err, "b.md")
		})
	}
}

func TestLoadWorkflowsBoundsFilesAndRequiresDirectoryContents(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := Load(dir, 4096)
	require.ErrorContains(t, err, "at least one")
	valid := testFrontMatter + "---\nInstructions"
	writeWorkflow(t, dir, "review.md", valid)
	_, err = Load(dir, len(valid)-1)
	require.ErrorContains(t, err, "exceeds")
	_, err = Load(dir, len(valid))
	require.NoError(t, err)
	_, err = Load(filepath.Join(dir, "missing"), 4096)
	require.Error(t, err)
}

func TestLanguageOnlyAddsOutputDirectiveWhenConfigured(t *testing.T) {
	t.Parallel()
	for _, language := range []string{"", "ru", "pt-BR"} {
		options := contractOptions()
		options.Language = language
		contract, err := BuildSessionContract(contractReviewInput(), options)
		require.NoError(t, err)
		instructions := *contract.Request.Configuration.Agent.Instructions
		if language == "" {
			require.Equal(t, testInstructions, instructions)
		} else {
			require.True(t, strings.HasPrefix(instructions, testInstructions))
			require.Contains(t, instructions, "Review output language: "+language)
			for _, output := range []string{"finding titles and bodies", "recommendations", "resolution explanations", "recurrence comments", "final response"} {
				require.Contains(t, instructions, output)
			}
		}
	}
}

func TestRevisionBindsEffectiveWorkflowSettingsWithoutChangingSessionIdentity(t *testing.T) {
	t.Parallel()
	original, err := BuildSessionContract(contractReviewInput(), contractOptions())
	require.NoError(t, err)
	mutations := []func(*Options){
		func(o *Options) { o.WorkflowID = "another-workflow" },
		func(o *Options) { o.Instructions += "New rules" },
		func(o *Options) { o.AgentProfile = "other" },
		func(o *Options) { o.AgentModel = "other" },
		func(o *Options) { o.SandboxTemplate = "other" },
		func(o *Options) { o.Services = []string{} },
		func(o *Options) { o.Language = "ru" },
		func(o *Options) { o.RunTimeoutSeconds++ },
		func(o *Options) { o.HookTimeoutSeconds++ },
	}
	for _, mutate := range mutations {
		options := contractOptions()
		mutate(&options)
		changed, err := BuildSessionContract(contractReviewInput(), options)
		require.NoError(t, err)
		require.NotEqual(t, original.WorkflowRevision, changed.WorkflowRevision)
		require.Equal(t, original.Key, changed.Key)
		require.Equal(t, *original.Request.Messages[0].ExternalKey, *changed.Request.Messages[0].ExternalKey)
		require.NoError(t, ValidateMetadata(original.Metadata, contractReviewInput()), "recovery uses frozen metadata")
	}
}

func TestDocumentedWorkflowExamplesLoad(t *testing.T) {
	t.Parallel()
	workflows, err := Load("../../examples/workflows", 4096)
	require.NoError(t, err)
	for _, name := range []string{"team/api", "team/web", "Other-Team/API"} {
		selected, ok := workflows.Select(name)
		require.True(t, ok)
		require.Equal(t, "selected-repositories", selected.ID)
	}
	selected, ok := workflows.Select("team/another")
	require.True(t, ok)
	require.Equal(t, "default-review", selected.ID)
}
