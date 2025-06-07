package main

import (
	"context"
	"crypto/rand"   // For random commit selection
	"encoding/json" // For Gemini API response parsing
	"flag"
	"fmt"
	"log"
	"math/big" // For random commit selection
	"os"
	"regexp" // For parsing Gemini response
	"sort"   // For sorting final scores
	"strings"
	// "time" // No longer explicitly used after removing rand.Seed

	"github.com/google/generative-ai-go/genai" // Gemini API client
	"github.com/google/go-github/v57/github"
	"golang.org/x/oauth2"
	"google.golang.org/api/option" // For Gemini API client options
)

// CommitInfo は分析対象のコミット詳細を保持
type CommitInfo struct {
	SHA     string
	Message string
	Diff    string // 先頭N行のdiff
}

// GeminiScore はGemini APIからの評価結果を保持する構造体
type GeminiScore struct {
	TechnicalSophistication int `json:"technical_sophistication"` // 1-10
	MessageAppropriateness  int `json:"message_appropriateness"`  // 1-10
}

// RepoScore はリポジトリの評価情報を保持する構造体
type RepoScore struct {
	Name                 string
	CommitCount          int
	TechnicalScore       int          // Geminiからの評価
	MessageScore         int          // Geminiからの評価
	OverallScore         float64      // 加重平均などで計算
	AnalyzedCommitsCount int          // 分析対象となったコミット数
	SampledCommits       []CommitInfo // 分析に使用したコミットのサンプル
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

// GeminiClient はGemini APIのクライアントをラップする構造体
type GeminiClient struct {
	client *genai.GenerativeModel
	ctx    context.Context
	apiKey string
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

// NewGeminiClient は新しいGeminiクライアントを作成
func NewGeminiClient(apiKey string) (*GeminiClient, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY is required. Please set it as an environment variable")
	}
	ctx := context.Background()
	client, err := genai.NewClient(ctx, option.WithAPIKey(apiKey))
	if err != nil {
		return nil, fmt.Errorf("failed to create genai client: %w", err)
	}
	// For text-only input, use a relevant model. gemini-1.5-flash-latest is a good general-purpose choice.
	model := client.GenerativeModel("gemini-1.5-flash-latest")
	// Consider setting temperature for more deterministic responses if needed
	// model.SafetySettings = []*genai.SafetySetting{
	// 	{
	// 		Category:  genai.HarmCategoryHarassment,
	// 		Threshold: genai.HarmBlockNone,
	// 	},
	// 	{
	// 		Category:  genai.HarmCategoryHateSpeech,
	// 		Threshold: genai.HarmBlockNone,
	// 	},
	// }

	return &GeminiClient{
		client: model,
		ctx:    ctx,
		apiKey: apiKey,
	}, nil
}

// AnalyzeCommitsWithGemini はコミット情報をGemini APIに送信して評価を取得
func (gc *GeminiClient) AnalyzeCommitsWithGemini(repoName string, totalCommitCount int, sampledCommits []CommitInfo) (*GeminiScore, error) {
	if totalCommitCount == 0 {
		return &GeminiScore{TechnicalSophistication: 0, MessageAppropriateness: 0}, nil
	}
	if len(sampledCommits) == 0 {
		// コミットはあるが分析対象のサンプルがない場合 (SHA取得失敗など)
		// 技術点は低め、メッセージ適切性は0とする
		log.Printf("No sampled commits to analyze for %s, assigning low scores.", repoName)
		return &GeminiScore{TechnicalSophistication: 1, MessageAppropriateness: 0}, nil
	}

	var analysisContent strings.Builder
	fmt.Fprintf(&analysisContent, "Repository: %s\n", repoName)
	fmt.Fprintf(&analysisContent, "Total Commits in Repository: %d\n\n", totalCommitCount)
	fmt.Fprintf(&analysisContent, "Analyzing %d randomly sampled commits:\n", len(sampledCommits))

	for i, sc := range sampledCommits {
		fmt.Fprintf(&analysisContent, "\n--- Sampled Commit %d ---\n", i+1)
		fmt.Fprintf(&analysisContent, "Commit Message:\n%s\n\n", sc.Message)
		fmt.Fprintf(&analysisContent, "Commit Diff (first 100 lines or less):\n%s\n", sc.Diff)
	}

	promptFormat := `
You are an expert code reviewer. Analyze the provided commit data for the repository named '%s'.
The data includes the total number of commits in the repository and a sample of %d individual commits, each with its commit message and a snippet of its diff (up to the first 100 lines).

Based ONLY on the provided information for these sampled commits, evaluate the following:

1.  **Technical Sophistication (1-10 points):**
    From the diff snippets of the sampled commits, assess the complexity of the changes, the use of advanced techniques or technologies, and the ingenuity in problem-solving.
    A score of 1 means very simple changes (e.g., typo fixes, minor documentation updates).
    A score of 10 means highly complex changes involving significant architectural work, advanced algorithms, or novel technology applications.
    If diffs are empty or uninformative, assign a low score.

2.  **Commit Message Appropriateness (1-10 points):**
    For each sampled commit, evaluate how well its commit message aligns with its corresponding diff snippet.
    Does the message accurately and concisely describe what was changed in the diff?
    A score of 1 means the message is irrelevant, misleading, or completely uninformative regarding the diff.
    A score of 10 means the message perfectly and clearly describes the changes shown in the diff.
    Consider the average appropriateness across all sampled commits.

Please provide your evaluation STRICTLY in the following JSON format, with no other text before or after the JSON block:
{
  "technical_sophistication": <integer_score_1_to_10_for_overall_repo_based_on_samples>,
  "message_appropriateness": <integer_score_1_to_10_for_average_message_quality_based_on_samples>
}

Analysis Data:
%s
`
	prompt := fmt.Sprintf(promptFormat, repoName, len(sampledCommits), analysisContent.String())

	// Log a snippet of the prompt for debugging
	log.Printf("Sending prompt to Gemini for repo %s (Total commits: %d, Sampled: %d). Prompt length: %d chars.\n", repoName, totalCommitCount, len(sampledCommits), len(prompt))
	if len(prompt) > 30000 { // Gemini Pro has a 32k token limit, Flash has 1M, but be mindful
		log.Printf("Warning: Prompt for %s is very long (%d chars), may exceed API limits or be slow.", repoName, len(prompt))
	}

	resp, err := gc.client.GenerateContent(gc.ctx, genai.Text(prompt))
	if err != nil {
		return nil, fmt.Errorf("failed to generate content from gemini for repo %s: %w", repoName, err)
	}

	if len(resp.Candidates) == 0 || resp.Candidates[0].Content == nil || len(resp.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("gemini response is empty or invalid for repo %s", repoName)
	}

	jsonResponse := ""
	if textPart, ok := resp.Candidates[0].Content.Parts[0].(genai.Text); ok {
		jsonResponse = string(textPart)
	} else {
		return nil, fmt.Errorf("unexpected response part type from gemini for repo %s: %T", repoName, resp.Candidates[0].Content.Parts[0])
	}

	log.Printf("Gemini raw response for %s: %s\n", repoName, jsonResponse)

	// Extract JSON from markdown code block if present, or directly if not in markdown
	re := regexp.MustCompile("(?s)```json\n(.*?)\n```")
	matches := re.FindStringSubmatch(jsonResponse)
	extractedJSON := ""
	if len(matches) > 1 {
		extractedJSON = strings.TrimSpace(matches[1])
	} else {
		// If not in markdown, try to find JSON directly.
		// This handles cases where Gemini might return plain JSON.
		// A more robust regex to find a JSON object.
		reJSON := regexp.MustCompile(`(?s)\s*\{\s*("technical_sophistication"|"message_appropriateness")[\s\S]*?\}\s*`)
		foundJSON := reJSON.FindString(jsonResponse)
		if foundJSON != "" {
			extractedJSON = strings.TrimSpace(foundJSON)
		} else {
			log.Printf("Could not find or extract JSON from Gemini response for %s. Raw: %s", repoName, jsonResponse)
			return nil, fmt.Errorf("could not extract JSON from Gemini response for %s. Raw: %s", repoName, jsonResponse)
		}
	}

	var score GeminiScore
	if err := json.Unmarshal([]byte(extractedJSON), &score); err != nil {
		log.Printf("Failed to parse extracted Gemini JSON response for %s. JSON attempted: '%s', Error: %v", repoName, extractedJSON, err)
		return nil, fmt.Errorf("failed to parse extracted gemini JSON response for %s, content: '%s': %w", repoName, extractedJSON, err)
	}

	return &score, nil
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

// GetRepositoryCommitAnalysisData fetches total commit count and details of N random commits (message + diff snippet).
// numRandomCommitsToAnalyze: 収集するランダムコミットの数
// diffLinesLimit: 各コミットのdiffから取得する最大行数
func (ghc *GitHubClient) GetRepositoryCommitAnalysisData(owner, repoName string, numRandomCommitsToAnalyze int, diffLinesLimit int) (totalCommits int, analyzedCommits []CommitInfo, err error) {
	// Go 1.20未満の場合や明示的なシードが必要な場合
	// rand.Seed(time.Now().UnixNano()) // crypto/rand を使うので不要

	// 1. Get total commit count
	totalCommits, err = ghc.countAllCommits(owner, repoName)
	if err != nil {
		// countAllCommitsが409でエラーなく0を返す場合があるので、それを考慮
		if err.Error() == fmt.Sprintf("failed to list commits for %s/%s during full count: GET https://api.github.com/repos/%s/%s/commits?per_page=100: 409  []", owner, repoName, owner, repoName) && totalCommits == 0 {
			// This specific error for empty repo means 0 commits, not a failure to list.
		} else if totalCommits == 0 && strings.Contains(err.Error(), "409") { // More general 409 check
			// Assume 0 commits if 409 and count is 0
		} else {
			return 0, nil, fmt.Errorf("error counting all commits for %s/%s: %w", owner, repoName, err)
		}
	}
	if totalCommits == 0 {
		return 0, []CommitInfo{}, nil
	}

	// 2. Get all commit SHAs (can be slow for very large repos)
	var allCommitSHAs []string
	listOpt := &github.CommitsListOptions{
		ListOptions: github.ListOptions{PerPage: 100}, // Fetch 100 SHAs per page
	}
	for {
		commitsPage, resp, listErr := ghc.client.Repositories.ListCommits(ghc.ctx, owner, repoName, listOpt)
		if listErr != nil {
			// If we fail to list commits here, but got a total count, return count with empty analysis.
			// This could happen if repo becomes empty between count and list, or other issues.
			log.Printf("Warning: Failed to list commit SHAs for %s/%s after getting count %d: %v. Proceeding with no sampled commits.", owner, repoName, totalCommits, listErr)
			return totalCommits, []CommitInfo{}, nil
		}
		for _, c := range commitsPage {
			if c.SHA != nil {
				allCommitSHAs = append(allCommitSHAs, *c.SHA)
			}
		}
		if resp.NextPage == 0 || len(allCommitSHAs) >= totalCommits { // Stop if no more pages or we have enough SHAs
			break
		}
		listOpt.Page = resp.NextPage
		if len(allCommitSHAs) > 500 && numRandomCommitsToAnalyze <= 10 { // Optimization: if repo is huge, don't fetch all SHAs if we only need a few
			log.Printf("Optimization: Fetched %d SHAs for %s/%s, stopping early as we only need %d samples.", len(allCommitSHAs), owner, repoName, numRandomCommitsToAnalyze)
			break
		}
	}

	if len(allCommitSHAs) == 0 {
		log.Printf("Warning: No commit SHAs found for %s/%s despite totalCommits = %d. Proceeding with no sampled commits.", owner, repoName, totalCommits)
		return totalCommits, []CommitInfo{}, nil
	}

	// 3. Select N random unique commit SHAs
	// selectedSHAsMap := make(map[string]bool) // No longer needed with Fisher-Yates
	var finalSelectedSHAs []string

	numToSelect := numRandomCommitsToAnalyze
	if len(allCommitSHAs) < numRandomCommitsToAnalyze {
		numToSelect = len(allCommitSHAs) // Cannot select more than available
	}

	// Fisher-Yates shuffle variant for selecting N random unique elements
	// Create a slice of indices
	indices := make([]int, len(allCommitSHAs))
	for i := range indices {
		indices[i] = i
	}
	// Shuffle the indices
	for i := len(indices) - 1; i > 0; i-- {
		jBig, _ := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		j := int(jBig.Int64())
		indices[i], indices[j] = indices[j], indices[i]
	}
	// Take the first numToSelect shuffled indices to get random SHAs
	for i := 0; i < numToSelect; i++ {
		finalSelectedSHAs = append(finalSelectedSHAs, allCommitSHAs[indices[i]])
	}

	// 4. Fetch details (message, diff) for selected SHAs
	for _, sha := range finalSelectedSHAs {
		// Get the commit details including files and patch
		commit, _, getErr := ghc.client.Repositories.GetCommit(ghc.ctx, owner, repoName, sha, &github.ListOptions{})
		if getErr != nil {
			log.Printf("Error getting commit details for %s (SHA: %s): %v. Skipping this commit.", repoName, sha, getErr)
			continue
		}

		var message string
		if commit.Commit != nil && commit.Commit.Message != nil {
			message = *commit.Commit.Message
		}

		var diffSnippet strings.Builder
		currentLines := 0
		if commit.Files != nil {
			for _, file := range commit.Files {
				if file.GetPatch() != "" { // Patch contains the diff
					if diffLinesLimit > 0 { // Only process if limit is positive
						patchLines := strings.Split(file.GetPatch(), "\n")
						for _, line := range patchLines {
							if currentLines >= diffLinesLimit {
								diffSnippet.WriteString("\n... (diff truncated due to line limit)\n")
								goto EndDiffProcessing // Break out of nested loops
							}
							diffSnippet.WriteString(line)
							diffSnippet.WriteString("\n")
							currentLines++
						}
						if currentLines < diffLinesLimit { // Add separator if not truncated yet and more files might come
							diffSnippet.WriteString("---\n") // Separator between file diffs
						}
					} else { // No line limit, take full patch for this file
						diffSnippet.WriteString(file.GetPatch())
						diffSnippet.WriteString("\n---\n")
					}
				}
			}
		}
	EndDiffProcessing:

		analyzedCommits = append(analyzedCommits, CommitInfo{
			SHA:     sha,
			Message: message,
			Diff:    strings.TrimSuffix(diffSnippet.String(), "\n---\n"), // Clean up trailing separator
		})
	}
	log.Printf("Fetched %d commit details for analysis for repo %s/%s", len(analyzedCommits), owner, repoName)
	return totalCommits, analyzedCommits, nil
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

const (
	numRandomCommitsToAnalyze = 5   // Number of random commits to analyze per repo
	diffLinesLimit            = 100 // Max lines of diff per commit file for analysis
	// Weights for overall score calculation
	commitCountWeight     = 0.2
	technicalScoreWeight  = 0.4
	messageScoreWeight    = 0.4
	maxCommitCountForNorm = 1000 // For normalizing commit count score (cap at this)
)

func main() {
	cfg := parseFlags()

	geminiAPIKey := os.Getenv("GEMINI_API_KEY")
	if geminiAPIKey == "" {
		log.Fatal("Error: GEMINI_API_KEY environment variable not set.")
	}

	ghClient := NewGitHubClient(cfg.Token)
	geminiClient, err := NewGeminiClient(geminiAPIKey)
	if err != nil {
		log.Fatalf("Error creating Gemini client: %v\n", err)
	}

	log.Printf("Fetching repositories for organization: %s\n", cfg.Organization)
	repos, err := ghClient.GetOrganizationRepos(cfg.Organization)
	if err != nil {
		log.Fatalf("Error: %v\n", err)
	}

	if len(repos) == 0 {
		log.Printf("No repositories found for organization %s.\n", cfg.Organization)
		return
	}

	log.Printf("Found %d repositories. Analyzing each repository...\n", len(repos))

	var repoScores []RepoScore

	for i, repo := range repos {
		repoName := repo.GetName()
		if repoName == "" {
			log.Printf("Skipping repository with no name (index %d)", i)
			continue
		}
		log.Printf("Analyzing repository: %s (%d/%d)", repoName, i+1, len(repos))

		totalRepoCommits, sampledCommits, err := ghClient.GetRepositoryCommitAnalysisData(cfg.Organization, repoName, numRandomCommitsToAnalyze, diffLinesLimit)
		if err != nil {
			log.Printf("Error getting commit analysis data for %s: %v. Skipping this repo.", repoName, err)
			repoScores = append(repoScores, RepoScore{Name: repoName, CommitCount: -1}) // Mark as errored
			continue
		}

		if totalRepoCommits == 0 {
			log.Printf("Repository %s has 0 commits. Skipping Gemini analysis.", repoName)
			repoScores = append(repoScores, RepoScore{Name: repoName, CommitCount: 0, TechnicalScore: 0, MessageScore: 0, OverallScore: 0, AnalyzedCommitsCount: 0})
			continue
		}

		geminiEval, err := geminiClient.AnalyzeCommitsWithGemini(repoName, totalRepoCommits, sampledCommits)
		if err != nil {
			log.Printf("Error analyzing commits with Gemini for %s: %v. Assigning default scores.", repoName, err)
			// Assign default/error scores if Gemini fails
			repoScores = append(repoScores, RepoScore{
				Name:                 repoName,
				CommitCount:          totalRepoCommits,
				TechnicalScore:       0, // Or some error indicator like -1
				MessageScore:         0, // Or some error indicator like -1
				OverallScore:         0,
				AnalyzedCommitsCount: len(sampledCommits),
				SampledCommits:       sampledCommits,
			})
			continue
		}

		// Normalize commit count score (0-10)
		normalizedCommitCount := float64(totalRepoCommits)
		if normalizedCommitCount > float64(maxCommitCountForNorm) {
			normalizedCommitCount = float64(maxCommitCountForNorm)
		}
		commitScore := (normalizedCommitCount / float64(maxCommitCountForNorm)) * 10.0

		// Calculate overall score
		overallScore := (commitScore * commitCountWeight) +
			(float64(geminiEval.TechnicalSophistication) * technicalScoreWeight) +
			(float64(geminiEval.MessageAppropriateness) * messageScoreWeight)

		repoScores = append(repoScores, RepoScore{
			Name:                 repoName,
			CommitCount:          totalRepoCommits,
			TechnicalScore:       geminiEval.TechnicalSophistication,
			MessageScore:         geminiEval.MessageAppropriateness,
			OverallScore:         overallScore,
			AnalyzedCommitsCount: len(sampledCommits),
			SampledCommits:       sampledCommits,
		})
		// Optional: Add a small delay to avoid hitting API rate limits too quickly if many repos
		// time.Sleep(1 * time.Second)
	}

	// Sort repositories by OverallScore in descending order
	sort.SliceStable(repoScores, func(i, j int) bool {
		return repoScores[i].OverallScore > repoScores[j].OverallScore
	})

	// Print results
	fmt.Println(strings.Repeat("=", 120))
	fmt.Printf("%-40s | %-10s | %-10s | %-10s | %-10s | %-15s\n", "Repository", "Commits", "Tech Score", "Msg Score", "Overall", "Analyzed Smpls")
	fmt.Println(strings.Repeat("-", 120))
	for _, rs := range repoScores {
		if rs.CommitCount == -1 { // Error case
			fmt.Printf("%-40s | %-10s | %-10s | %-10s | %-10s | %-15s\n", rs.Name, "ERROR", "N/A", "N/A", "N/A", "N/A")
			continue
		}
		fmt.Printf("%-40s | %-10d | %-10d | %-10d | %-10.2f | %-15d\n",
			rs.Name, rs.CommitCount, rs.TechnicalScore, rs.MessageScore, rs.OverallScore, rs.AnalyzedCommitsCount)
	}
	fmt.Println(strings.Repeat("=", 120))

	// Optional: Print details of sampled commits for top N repositories
	// printTopNRepoSamples(repoScores, 3)
}

// func printTopNRepoSamples(scores []RepoScore, topN int) {
//  log.Println("\n\n--- Details of Sampled Commits for Top Repositories ---")
//  for i := 0; i < min(topN, len(scores)); i++ {
//      rs := scores[i]
//      if rs.CommitCount == -1 || rs.AnalyzedCommitsCount == 0 {
//          continue
//      }
//      fmt.Printf("\n\nRepository: %s (Overall Score: %.2f)\n", rs.Name, rs.OverallScore)
//      fmt.Printf("Analyzed %d sampled commits:\n", rs.AnalyzedCommitsCount)
//      for j, sc := range rs.SampledCommits {
//          fmt.Printf("  Sample %d (SHA: %s):\n", j+1, sc.SHA)
//          fmt.Printf("    Message: %s\n", strings.Split(sc.Message, "\n")[0]) // First line of message
//          fmt.Printf("    Diff Snippet (first 3 lines):\n      %s\n", strings.Join(strings.Split(sc.Diff, "\n")[:min(len(strings.Split(sc.Diff, "\n")),3)], "\n      "))
//      }
//  }
// }
