package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/google/go-github/v57/github"
	"golang.org/x/oauth2"
	"image/color" // For plot colors
	"sort"

	"gonum.org/v1/plot"
	"gonum.org/v1/plot/plotter"
	"gonum.org/v1/plot/vg"
)

// RepoCommitCount はリポジトリ名とコミット数を保持する構造体
type RepoCommitCount struct {
	Name        string
	CommitCount int
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Config はアプリケーションの設定を保持する構造体
type Config struct {
	Organization string
	Token        string
}

// GitHubClient はGitHub APIのクライアントをラップする構造体
type GitHubClient struct {
	client *github.Client
	ctx    context.Context
}

// NewGitHubClient は新しいGitHubクライアントを作成
func NewGitHubClient(token string) *GitHubClient {
	ctx := context.Background()
	var ghClient *github.Client

	if token != "" {
		ts := oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: token},
		)
		tc := oauth2.NewClient(ctx, ts)
		ghClient = github.NewClient(tc)
	} else {
		ghClient = github.NewClient(nil)
	}

	return &GitHubClient{
		client: ghClient,
		ctx:    ctx,
	}
}

// GetOrganizationRepos は指定されたOrganizationのリポジトリ一覧を取得
func (ghc *GitHubClient) GetOrganizationRepos(orgName string) ([]*github.Repository, error) {
	var allRepos []*github.Repository
	opt := &github.RepositoryListByOrgOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}

	for {
		repos, resp, err := ghc.client.Repositories.ListByOrg(ghc.ctx, orgName, opt)
		if err != nil {
			return nil, fmt.Errorf("failed to list repositories for organization %s: %w", orgName, err)
		}
		allRepos = append(allRepos, repos...)
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return allRepos, nil
}

// GetRepositoryCommitCount は指定されたリポジトリのコミット数を取得
// 注意: この関数はデフォルトブランチのコミット数を取得しようとします。
// GitHub API v3では、全ブランチの合計コミット数を直接取得する簡単な方法は提供されていません。
// ここでは、リポジトリのコミットリストAPIを利用し、最初のコミットと最後のコミットの情報を取得することで、
// おおよそのコミット数を推定するか、ページネーションを利用して全コミットを数えます。
// より正確な数を取得するには、すべてのコミットをページネーションで取得する必要がありますが、
// 大規模なリポジトリではAPIレート制限に達する可能性があります。
// ここでは簡略化のため、コミットリストの最初のページの情報を利用します。
// より堅牢な実装では、`ListCommits`のページネーションを処理する必要があります。
func (ghc *GitHubClient) GetRepositoryCommitCount(owner, repoName string) (int, error) {
	opt := &github.CommitsListOptions{
		ListOptions: github.ListOptions{PerPage: 1}, // 最初の1件だけ取得してヘッダーを見る
	}

	// HEADを指定してデフォルトブランチのコミットを取得
	commits, resp, err := ghc.client.Repositories.ListCommits(ghc.ctx, owner, repoName, opt)
	if err != nil {
		// リポジトリが空の場合など
		if resp != nil && resp.StatusCode == 409 {
			return 0, nil // 空のリポジトリはコミット0
		}
		return 0, fmt.Errorf("failed to list commits for %s/%s: %w", owner, repoName, err)
	}

	if len(commits) == 0 {
		return 0, nil // コミットがない場合
	}

	// Linkヘッダーから最後のページ番号を取得して総コミット数を推定
	// 例: <https://api.github.com/repositories/123/commits?page=2>; rel="next", <https://api.github.com/repositories/123/commits?page=34>; rel="last"
	if resp.LastPage > 0 {
		// 1ページあたりのコミット数はAPIによって異なる場合があるが、ここではListOptions.PerPageで指定した値(1)ではなく、
		// GitHubのデフォルトのper_page (通常30) で計算されることが多い。
		// より正確には、実際に全ページを取得する必要がある。
		// ここでは簡略化のため、LastPage * (デフォルトのper_page) とする。
		// ただし、ListCommitsのデフォルトは30件なので、それで計算する。
		// もしPerParge=1でLastPageが得られるなら、それが総数に近い。
		// しかし、通常はPerParge=1でLastPageは得られないか、得られても1になる。
		// そのため、全件取得するロジックに切り替える。
		return ghc.countAllCommits(owner, repoName)
	}

	// Linkヘッダーがない場合 (コミットが1ページに収まる場合)
	return ghc.countAllCommits(owner, repoName)
}

func (ghc *GitHubClient) countAllCommits(owner, repoName string) (int, error) {
	var commitCount int
	opt := &github.CommitsListOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}
	for {
		commits, resp, err := ghc.client.Repositories.ListCommits(ghc.ctx, owner, repoName, opt)
		if err != nil {
			if resp != nil && resp.StatusCode == 409 { // Conflict, e.g. empty repository
				return 0, nil
			}
			return 0, fmt.Errorf("failed to list commits for %s/%s during full count: %w", owner, repoName, err)
		}
		commitCount += len(commits)
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return commitCount, nil
}

// GetRepositoryCommitAnalysisData is no longer needed for the simple bar chart.
// func (ghc *GitHubClient) GetRepositoryCommitAnalysisData(...) { ... }

func parseFlags() *Config {
	orgName := flag.String("org", "", "GitHub Organization name (required)")
	token := flag.String("token", os.Getenv("GITHUB_TOKEN"), "GitHub Personal Access Token (optional, defaults to GITHUB_TOKEN env var)")

	flag.Parse()

	if *orgName == "" {
		log.Println("Error: Organization name is required.")
		flag.Usage()
		os.Exit(1)
	}

	return &Config{
		Organization: *orgName,
		Token:        *token,
	}
}

func main() {
	cfg := parseFlags()

	ghClient := NewGitHubClient(cfg.Token)

	log.Printf("Fetching repositories for organization: %s\n", cfg.Organization)
	repos, err := ghClient.GetOrganizationRepos(cfg.Organization)
	if err != nil {
		log.Fatalf("Error: %v\n", err)
	}

	if len(repos) == 0 {
		log.Printf("No repositories found for organization %s.\n", cfg.Organization)
		return
	}

	log.Printf("Found %d repositories. Fetching commit counts and generating ranking...\n", len(repos))

	var repoCommits []RepoCommitCount
	maxCommitsValue := 0 // To scale the bar chart

	for _, repo := range repos {
		repoName := repo.GetName()
		if repoName == "" {
			log.Println("Skipping repository with no name.")
			continue
		}

		log.Printf("Fetching commit count for %s...", repoName)
		// Use GetRepositoryCommitCount or countAllCommits directly.
		// GetRepositoryCommitCount internally calls countAllCommits if needed.
		commitCount, err := ghClient.GetRepositoryCommitCount(cfg.Organization, repoName)
		if err != nil {
			log.Printf("Could not get commit count for %s: %v. Marking as error.", repoName, err)
			repoCommits = append(repoCommits, RepoCommitCount{Name: repoName, CommitCount: -1}) // Mark as error
			continue
		}
		repoCommits = append(repoCommits, RepoCommitCount{Name: repoName, CommitCount: commitCount})
		if commitCount > maxCommitsValue {
			maxCommitsValue = commitCount
		}
	}

	// Sort by commit count in descending order
	sort.Slice(repoCommits, func(i, j int) bool {
		// Handle error case (-1) by pushing them to the bottom
		if repoCommits[i].CommitCount == -1 {
			return false
		}
		if repoCommits[j].CommitCount == -1 {
			return true
		}
		return repoCommits[i].CommitCount > repoCommits[j].CommitCount
	})

	// Generate and save bar chart
	if len(repoCommits) > 0 && maxCommitsValue > 0 {
		p := plot.New()

		p.Title.Text = fmt.Sprintf("Commit Count Ranking for %s", cfg.Organization)
		p.Y.Label.Text = "Commit Count"
		p.X.Label.Text = "Repositories"

		// Prepare data for bar chart
		var plotValues plotter.Values
		var labels []string
		// Consider limiting the number of repos to plot if there are too many for readability
		numReposToPlot := len(repoCommits)
		// if numReposToPlot > 20 { // Example limit
		//  numReposToPlot = 20
		//  log.Printf("Limiting bar chart to top %d repositories for readability.", numReposToPlot)
		// }

		for i := 0; i < numReposToPlot; i++ {
			rc := repoCommits[i]
			if rc.CommitCount < 0 { // Skip errored repos
				continue
			}
			plotValues = append(plotValues, float64(rc.CommitCount))

			// Process repository name to remove prefix
			displayName := rc.Name
			if idx := strings.IndexAny(displayName, "-_"); idx != -1 {
				// Check if there's anything after the prefix
				if idx+1 < len(displayName) {
					displayName = displayName[idx+1:]
				}
				// If the prefix is at the end or only prefix exists, keep original or decide on a policy
				// For now, if only prefix or prefix at end, it will effectively take the part after it (empty or original)
				// A more robust way might be to ensure the part after prefix is substantial.
			}
			labels = append(labels, displayName)
		}

		if len(plotValues) > 0 {
			bars, err := plotter.NewBarChart(plotValues, vg.Points(20)) // Adjust bar width as needed
			if err != nil {
				log.Fatalf("Could not create bar chart: %v", err)
			}
			bars.LineStyle.Width = vg.Length(0)     // No border for bars
			bars.Color = color.RGBA{B: 128, A: 255} // Blue bars
			bars.Horizontal = false                 // Vertical bars

			p.Add(bars)
			p.NominalX(labels...)
			p.HideY() // Hide Y axis numbers if labels are directly on bars or too cluttered
			// Or, to show Y-axis ticks:
			// p.Y.Tick.Marker = plot.DefaultTicks{}

			// Rotate X-axis labels for better readability if many repos
			p.X.Tick.Label.Rotation = 0.8 // Radians, approx 45 degrees
			// p.X.Tick.Label.YAlign = vg.AlignTop // Temporarily commented out due to undefined error
			// p.X.Tick.Label.XAlign = vg.AlignRight // Temporarily commented out due to undefined error
			// Default alignment will be used. If labels overlap, consider adjusting Rotation or plot width.

			// Save the plot to a PNG file.
			// Adjust width based on number of labels for better readability
			plotWidth := vg.Length(len(labels)) * 1.5 * vg.Inch // Dynamic width
			if plotWidth < 6*vg.Inch {
				plotWidth = 6 * vg.Inch // Minimum width
			}
			if plotWidth > 20*vg.Inch {
				plotWidth = 20 * vg.Inch // Maximum width
			}
			plotHeight := 6 * vg.Inch

			if err := p.Save(plotWidth, plotHeight, "commits_barchart.png"); err != nil {
				log.Fatalf("Could not save plot: %v", err)
			}
			log.Println("Bar chart saved to commits_barchart.png")
		} else {
			log.Println("No valid data to plot for bar chart.")
		}
	} else {
		log.Println("No repositories with commits found to generate a chart.")
	}

	// Print simple table as well (optional, can be removed if chart is primary output)
	fmt.Println(strings.Repeat("=", 80))
	fmt.Printf("Commit Count Ranking for Organization: %s (Data also in commits_barchart.png)\n", cfg.Organization)
	fmt.Println(strings.Repeat("-", 80))
	fmt.Printf("%-45s | %s\n", "Repository", "Commits")
	fmt.Println(strings.Repeat("-", 80))
	for _, rc := range repoCommits {
		if rc.CommitCount == -1 {
			fmt.Printf("%-45s | %s\n", rc.Name, "ERROR")
			continue
		}
		fmt.Printf("%-45s | %d\n", rc.Name, rc.CommitCount)
	}
	fmt.Println(strings.Repeat("=", 80))

	// Calculate and print total and average
	totalOrgCommits := 0
	validRepoCount := 0
	for _, rc := range repoCommits {
		if rc.CommitCount != -1 {
			totalOrgCommits += rc.CommitCount
			validRepoCount++
		}
	}

	if validRepoCount > 0 {
		avgCommits := float64(totalOrgCommits) / float64(validRepoCount)
		fmt.Printf("Total commits across %d valid repositories: %d\n", validRepoCount, totalOrgCommits)
		fmt.Printf("Average commits per valid repository: %.2f\n", avgCommits)
	} else {
		fmt.Println("No valid repository data to calculate total or average commits.")
	}
	fmt.Println(strings.Repeat("=", 80))
}
