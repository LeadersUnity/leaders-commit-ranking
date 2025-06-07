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
)

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

	log.Printf("Found %d repositories. Fetching commit counts...\n", len(repos))
	fmt.Println(strings.Repeat("=", 50))
	fmt.Printf("%-40s | %s\n", "Repository", "Commit Count")
	fmt.Println(strings.Repeat("-", 50))

	totalCommits := 0
	for _, repo := range repos {
		if repo.GetName() == "" { // まれにNameが空のことがあるかもしれない
			continue
		}
		commitCount, err := ghClient.GetRepositoryCommitCount(cfg.Organization, repo.GetName())
		if err != nil {
			log.Printf("Could not get commit count for %s: %v\n", repo.GetName(), err)
			continue
		}
		fmt.Printf("%-40s | %d\n", repo.GetName(), commitCount)
		totalCommits += commitCount
	}

	fmt.Println(strings.Repeat("=", 50))
	fmt.Printf("%-40s | %d\n", "Total Commits", totalCommits)
	if len(repos) > 0 {
		avgCommits := float64(totalCommits) / float64(len(repos))
		fmt.Printf("%-40s | %.2f\n", "Average Commits per Repo", avgCommits)
	}
	fmt.Println(strings.Repeat("=", 50))
}
