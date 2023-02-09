package repository

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/MakeNowJust/heredoc/v2"
	"github.com/azure/azure-dev/cli/azd/pkg/environment/azdcontext"
	"github.com/azure/azure-dev/cli/azd/pkg/exec"
	"github.com/azure/azure-dev/cli/azd/pkg/input"
	"github.com/azure/azure-dev/cli/azd/pkg/project"
	"github.com/azure/azure-dev/cli/azd/pkg/tools/git"
	"github.com/azure/azure-dev/cli/azd/test/mocks/mockexec"
	"github.com/azure/azure-dev/cli/azd/test/mocks/mockinput"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/slices"
)

func Test_Initializer_Initialize(t *testing.T) {
	tests := []struct {
		name        string
		templateDir string
		// Files that will be mocked to be executable when fetched remotely.
		// Equally, these files are asserted to be executable after init.
		executableFiles []string
	}{
		{"RegularTemplate", "template", []string{"script/test.sh"}},
		{"MinimalTemplate", "template-minimal", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projectDir := t.TempDir()
			ctx := context.Background()
			azdCtx := azdcontext.NewAzdContextWithDirectory(projectDir)
			console := mockinput.NewMockConsole()
			realRunner := exec.NewCommandRunner(os.Stdin, os.Stdout, os.Stderr)
			mockRunner := mockexec.NewMockCommandRunner()
			mockRunner.When(func(args exec.RunArgs, command string) bool { return true }).
				RespondFn(func(args exec.RunArgs) (exec.RunResult, error) {
					// Stub out git clone, otherwise run actual command
					if slices.Contains(args.Args, "clone") && slices.Contains(args.Args, "local") {
						stagingDir := args.Args[len(args.Args)-1]
						copyTemplate(t, testDataPath(tt.templateDir), stagingDir)

						gitArgs := exec.NewRunArgs("git", "-C", stagingDir).WithEnrichError(true)

						// Mock clone by creating a git repository locally
						_, err := realRunner.Run(ctx, gitArgs.AppendParams("init"))
						require.NoError(t, err)

						_, err = realRunner.Run(ctx, gitArgs.AppendParams("add", "*"))
						require.NoError(t, err)

						for _, file := range tt.executableFiles {
							_, err = realRunner.Run(
								ctx,
								gitArgs.AppendParams("update-index", "--chmod=+x", file))
							require.NoError(t, err)

							// Mocks the correct behavior in *nix when the file lands on the filesystem.
							// git would have automatically set the correct file executable permissions.
							//
							// Note that `git update-index --chmod=+x` simply updates the tracked permissions in git,
							// but does not update the files directly, hence this is needed.
							if runtime.GOOS != "windows" {
								err = os.Chmod(filepath.Join(stagingDir, file), 0755)
								require.NoError(t, err)
							}
						}

						return exec.NewRunResult(0, "", ""), nil
					}

					return realRunner.Run(ctx, args)
				})

			i := NewInitializer(console, git.NewGitCli(mockRunner))
			err := i.Initialize(ctx, azdCtx, "local", "")
			require.NoError(t, err)

			verifyTemplateCopied(t, testDataPath(tt.templateDir), projectDir)
			verifyExecutableFilePermissions(t, ctx, i.gitCli, projectDir, tt.executableFiles)

			require.FileExists(t, filepath.Join(projectDir, ".gitignore"))
			require.FileExists(t, azdCtx.ProjectPath())
			require.DirExists(t, azdCtx.EnvironmentDirectory())
		})
	}
}

func Test_Initializer_InitializeWithOverwritePrompt(t *testing.T) {
	templateDir := "template"
	tests := []struct {
		name             string
		confirmOverwrite bool
	}{
		{"Confirm", true},
		{"Deny", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projectDir := t.TempDir()
			azdCtx := azdcontext.NewAzdContextWithDirectory(projectDir)
			// Copy all files to project to set up duplicate files
			copyTemplate(t, testDataPath(templateDir), projectDir)

			console := mockinput.NewMockConsole()
			console.WhenConfirm(func(options input.ConsoleOptions) bool {
				return strings.Contains(options.Message, "Overwrite files with versions from template?")
			}).Respond(tt.confirmOverwrite)

			realRunner := exec.NewCommandRunner(os.Stdin, os.Stdout, os.Stderr)
			mockRunner := mockexec.NewMockCommandRunner()
			mockRunner.When(func(args exec.RunArgs, command string) bool { return true }).
				RespondFn(func(args exec.RunArgs) (exec.RunResult, error) {
					// Stub out git clone, otherwise run actual command
					if slices.Contains(args.Args, "clone") && slices.Contains(args.Args, "local") {
						stagingDir := args.Args[len(args.Args)-1]
						copyTemplate(t, testDataPath(templateDir), stagingDir)
						_, err := realRunner.Run(context.Background(), exec.NewRunArgs("git", "-C", stagingDir, "init"))
						require.NoError(t, err)

						return exec.NewRunResult(0, "", ""), nil
					}

					return realRunner.Run(context.Background(), args)
				})

			i := NewInitializer(console, git.NewGitCli(mockRunner))
			err := i.Initialize(context.Background(), azdCtx, "local", "")

			if !tt.confirmOverwrite {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)

			verifyTemplateCopied(t, testDataPath(templateDir), projectDir)

			require.FileExists(t, filepath.Join(projectDir, ".gitignore"))
			require.FileExists(t, azdCtx.ProjectPath())
			require.DirExists(t, azdCtx.EnvironmentDirectory())
		})
	}
}

// Copy all files from source to target, removing *.txt suffix.
func copyTemplate(t *testing.T, source string, target string) {
	err := filepath.WalkDir(source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			require.NoError(t, err)
		}

		if d.IsDir() {
			relDir, err := filepath.Rel(source, path)
			if err != nil {
				return fmt.Errorf("computing relative path: %w", err)
			}

			return os.MkdirAll(filepath.Join(target, relDir), 0755)
		}

		rel, err := filepath.Rel(source, path)
		if err != nil {
			return fmt.Errorf("computing relative path: %w", err)
		}

		relTarget := strings.TrimSuffix(rel, ".txt")
		copyFile(t, filepath.Join(source, rel), filepath.Join(target, relTarget))

		return nil
	})

	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(target, ".git"), 0755))
}

// Verify all template code was copied to the destination.
func verifyTemplateCopied(t *testing.T, original string, copied string) {
	err := filepath.WalkDir(original, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			require.NoError(t, err)
		}

		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(original, path)
		if err != nil {
			return fmt.Errorf("computing relative path: %w", err)
		}

		relCopied := strings.TrimSuffix(rel, ".txt")
		verifyFileContent(t, filepath.Join(copied, relCopied), readFile(t, filepath.Join(original, rel)))

		return nil
	})

	require.NoError(t, err)
}

func verifyExecutableFilePermissions(t *testing.T,
	ctx context.Context,
	git git.GitCli,
	repoPath string,
	expectedFiles []string) {
	output, err := git.ListStagedFiles(ctx, repoPath)
	require.NoError(t, err)

	// On windows, since the file system doesn't keep track of executable file permissions,
	// we have to query git instead for the tracked permissions.
	if runtime.GOOS == "windows" {
		actual, err := parseExecutableFiles(output)
		require.NoError(t, err)

		require.ElementsMatch(t, actual, expectedFiles)

	} else {
		for _, file := range expectedFiles {
			fi, err := os.Stat(filepath.Join(repoPath, file))
			require.NoError(t, err)
			mode := fi.Mode()
			isExecutable := mode&0111 == 0111
			require.Truef(t, isExecutable, "file is not executable for all, fileMode: %s", mode)
		}
	}
}

func Test_Initializer_InitializeEmpty(t *testing.T) {
	type setup struct {
		projectFile   string
		gitignoreFile string
		gitIgnoreCrlf bool
	}

	type expected struct {
		projectFile   string
		gitignoreFile string
	}

	tests := []struct {
		name     string
		setup    setup
		expected expected
	}{
		{"CreateAll",
			setup{"", "", false},
			expected{projectFile: "azureyaml_created.txt", gitignoreFile: "gitignore_created.txt"}},
		{"AppendGitignore",
			setup{"azureyaml_existing.txt", "gitignore_existing.txt", false},
			expected{projectFile: "azureyaml_existing.txt", gitignoreFile: "gitignore_with_env.txt"}},
		{"AppendGitignoreNoTrailing",
			setup{"azureyaml_existing.txt", "gitignore_existing_notrail.txt", false},
			expected{projectFile: "azureyaml_existing.txt", gitignoreFile: "gitignore_with_env.txt"}},
		{"AppendGitignoreCrlf",
			setup{"azureyaml_existing.txt", "gitignore_existing.txt", true},
			expected{projectFile: "azureyaml_existing.txt", gitignoreFile: "gitignore_with_env.txt"}},
		{"AppendGitignoreNoTrailingCrlf",
			setup{"azureyaml_existing.txt", "gitignore_existing_notrail.txt", true},
			expected{projectFile: "azureyaml_existing.txt", gitignoreFile: "gitignore_with_env.txt"}},
		{"Unmodified",
			setup{"azureyaml_existing.txt", "gitignore_with_env.txt", false},
			expected{projectFile: "azureyaml_existing.txt", gitignoreFile: "gitignore_with_env.txt"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projectDir := t.TempDir()
			azdCtx := azdcontext.NewAzdContextWithDirectory(projectDir)

			if tt.setup.gitignoreFile != "" {
				if tt.setup.gitIgnoreCrlf {
					copyFileCrlf(t, testDataPath("empty", tt.setup.gitignoreFile), filepath.Join(projectDir, ".gitignore"))
				} else {
					copyFile(t, testDataPath("empty", tt.setup.gitignoreFile), filepath.Join(projectDir, ".gitignore"))
				}
			}

			if tt.setup.projectFile != "" {
				copyFile(t, testDataPath("empty", tt.setup.projectFile), azdCtx.ProjectPath())
			}

			console := mockinput.NewMockConsole()
			realRunner := exec.NewCommandRunner(os.Stdin, os.Stdout, os.Stderr)
			i := NewInitializer(console, git.NewGitCli(realRunner))
			err := i.InitializeEmpty(context.Background(), azdCtx)
			require.NoError(t, err)

			projectFileContent := readFile(t, testDataPath("empty", tt.expected.projectFile))
			gitIgnoreFileContent := readFile(t, testDataPath("empty", tt.expected.gitignoreFile))
			if tt.setup.gitIgnoreCrlf {
				gitIgnoreFileContent = crlf(gitIgnoreFileContent)
			}

			verifyProjectFile(t, azdCtx, projectFileContent)

			gitignore := filepath.Join(projectDir, ".gitignore")
			verifyFileContent(t, gitignore, gitIgnoreFileContent)

			require.DirExists(t, azdCtx.EnvironmentDirectory())
		})
	}
}

func testDataPath(elem ...string) string {
	elem = append([]string{"testdata"}, elem...)
	return filepath.Join(elem...)
}

func copyFile(t *testing.T, source string, target string) {
	content := readFile(t, source)
	err := os.WriteFile(target, []byte(content), 0644)

	require.NoError(t, err)
}

func copyFileCrlf(t *testing.T, source string, target string) {
	content := crlf(readFile(t, source))
	err := os.WriteFile(target, []byte(content), 0644)

	require.NoError(t, err)
}

func crlf(lfContent string) string {
	return strings.ReplaceAll(lfContent, "\n", "\r\n")
}

func readFile(t *testing.T, file string) string {
	bytes, err := os.ReadFile(file)
	require.NoError(t, err)
	content := string(bytes)

	return content
}

func verifyFileContent(t *testing.T, file string, content string) {
	require.FileExists(t, file)

	actualContent, err := os.ReadFile(file)
	require.NoError(t, err)
	require.Equal(t, content, string(actualContent))
}

func verifyProjectFile(t *testing.T, azdCtx *azdcontext.AzdContext, content string) {
	content = strings.Replace(content, "<project>", azdCtx.GetDefaultProjectName(), 1)
	verifyFileContent(t, azdCtx.ProjectPath(), content)

	_, err := project.LoadProjectConfig(azdCtx.ProjectPath())
	require.NoError(t, err)
}

func Test_determineDuplicates(t *testing.T) {
	type args struct {
		sourceFiles []string
		targetFiles []string
	}
	tests := []struct {
		name     string
		args     args
		expected []string
	}{
		{
			"NoDuplicates",
			args{[]string{"a.txt", "b.txt", "dir1/a.txt"}, []string{"c.txt", "d.txt", "dir2/a.txt"}},
			[]string{},
		},
		{"Duplicates", args{
			[]string{
				"a.txt", "b.txt", "c.txt",
				"dir1/a.txt",
				"dir1/dir2/b.txt", "dir1/dir2/d.txt"},
			[]string{
				"a.txt", "c.txt",
				"dir1/a.txt", "dir1/c.txt",
				"dir1/dir2/b.txt"}},
			[]string{
				"a.txt", "c.txt",
				"dir1/a.txt",
				"dir1/dir2/b.txt"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := t.TempDir()
			target := t.TempDir()

			createFiles(t, source, tt.args.sourceFiles)
			createFiles(t, target, tt.args.targetFiles)

			duplicates, err := determineDuplicates(source, target)

			expected := []string{}
			for _, expectedFile := range tt.expected {
				expected = append(expected, filepath.Clean(expectedFile))
			}

			assert.NoError(t, err)
			assert.ElementsMatch(t, duplicates, expected)
		})
	}
}

func createFiles(t *testing.T, dir string, files []string) {
	for _, file := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, file)), 0755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte{}, 0644))
	}
}

func Test_parseExecutableFiles(t *testing.T) {
	tests := []struct {
		name              string
		stagedFilesOutput string
		expected          []string
		expectErr         bool
	}{
		{
			"ParseSome",
			heredoc.Doc(`
				100755 0744dc7835515b7f6246969cc3a6d5ae69490db9 0	init.sh
				100755 0684640b0dad4297b21109f2a39a73f4b1e3ca41 0	script/script1.sh
				100644 8b41c35f177e442a80c3a9c3bac826d14628e6b4 0	readme.md
				100644 53f096183482e39868eecd1d1a54a2a17cbe72e6 0	src/any1.txt
				100755 0684640b0dad4297b21109f2a39a73f4b1e3ca41 0	script/script2.sh
				100644 7c6cfd932637e4e89ce03c79563ad4044bf5c030 0	src/any2.json
				100644 9b69faf15e1ba7232aa2004940ac3419bfe8192e 0	src/any3.csv
				100644 0a5ec605ae4bdfdf384780e1b713f9404d41d97f 0	src/any4.txt
				100755 de6afa7b4a15f3ef63a1756160a026e2284c514d 0	script/script3.sh
				100644 21df4a08f368817971d2b3da7f471b97874f572f 0	doc.md`),
			[]string{
				"init.sh",
				"script/script1.sh",
				"script/script2.sh",
				"script/script3.sh",
			},
			false,
		},
		{
			"ParseNone",
			heredoc.Doc(`
				100644 8b41c35f177e442a80c3a9c3bac826d14628e6b4 0	readme.md
				100644 53f096183482e39868eecd1d1a54a2a17cbe72e6 0	src/any1.txt
				100644 7c6cfd932637e4e89ce03c79563ad4044bf5c030 0	src/any2.json
				100644 9b69faf15e1ba7232aa2004940ac3419bfe8192e 0	src/any3.csv
				100644 0a5ec605ae4bdfdf384780e1b713f9404d41d97f 0	src/any4.txt
				100644 21df4a08f368817971d2b3da7f471b97874f572f 0	doc.md`),
			[]string{},
			false,
		},
		{"ParseEmpty", "", []string{}, false},
		{"ErrorInvalidFormat", "100755 de6afa7b4a15f3ef63a1756160a026e2284c514d", []string{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual, err := parseExecutableFiles(tt.stagedFilesOutput)

			if tt.expectErr {
				require.Error(t, err)
			} else {
				assert.Equal(t, tt.expected, actual)
			}
		})
	}
}