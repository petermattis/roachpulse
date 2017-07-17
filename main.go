package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codahale/hdrhistogram"
	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
)

var (
	cache = flag.String("c", filepath.Join(os.Getenv("HOME"), ".roachpulse"),
		"cached project data")
	update    = flag.Bool("u", false, "refresh cached project data")
	project   = flag.String("p", "cockroachdb/cockroach", "GitHub owner/repo name")
	tokenFile = flag.String("token", "",
		"read GitHub token personal access token from `file` (default $HOME/.github-issue-token)")
)

func prettyJSON(v interface{}) string {
	data, err := json.MarshalIndent(v, "", "\t")
	if err != nil {
		log.Fatal(err)
	}
	return string(data)
}

func saveJSON(path string, v interface{}) {
	data, err := json.MarshalIndent(v, "", "\t")
	if err != nil {
		log.Fatal(err)
	}
	if err := ioutil.WriteFile(path, data, 0666); err != nil {
		log.Fatal(err)
	}
}

func loadJSON(path string, v interface{}) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		log.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		log.Fatal(err)
	}
}

func makeClient() *github.Client {
	const short = ".github-issue-token"
	filename := filepath.Clean(os.Getenv("HOME") + "/" + short)
	shortFilename := filepath.Clean("$HOME/" + short)
	if *tokenFile != "" {
		filename = *tokenFile
		shortFilename = *tokenFile
	}
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		log.Fatal("reading token: ", err, "\n\n"+
			"Please create a personal access token at https://github.com/settings/tokens/new\n"+
			"and write it to ", shortFilename, " to use this program.\n"+
			"The token only needs the repo scope, or private_repo if you want to\n"+
			"view or edit issues for private repositories.\n"+
			"The benefit of using a personal access token over using your GitHub\n"+
			"password directly is that you can limit its use and revoke it at any time.\n\n")
	}
	fi, err := os.Stat(filename)
	if fi.Mode()&0077 != 0 {
		log.Fatalf("reading token: %s mode is %#o, want %#o", shortFilename, fi.Mode()&0777, fi.Mode()&0700)
	}
	// GitHub personal access token, from https://github.com/settings/applications.
	authToken := strings.TrimSpace(string(data))
	t := &oauth2.Transport{
		Source: &tokenSource{AccessToken: authToken},
	}
	return github.NewClient(&http.Client{Transport: t})
}

type tokenSource oauth2.Token

func (t *tokenSource) Token() (*oauth2.Token, error) {
	return (*oauth2.Token)(t), nil
}

// Issue ...
type Issue struct {
	github.Issue
	Timeline []*github.Timeline
	Commits  []*github.RepositoryCommit
}

func (i *Issue) save() {
	saveJSON(filepath.Join(*cache, fmt.Sprintf("%d", *i.Number)), i)
}

// Project ...
type Project struct {
	Owner       string
	Repo        string
	RefreshedAt time.Time

	issues     map[int]*Issue
	users      map[int]*github.User
	milestones map[int]*github.Milestone
	repos      map[int]*github.Repository
}

func makeProject(project string) *Project {
	f := strings.Split(project, "/")
	if len(f) != 2 {
		log.Fatal("invalid form for -p argument: must be owner/repo, like cockroachdb/cockroach")
	}
	return &Project{
		Owner:      f[0],
		Repo:       f[1],
		issues:     make(map[int]*Issue),
		users:      make(map[int]*github.User),
		milestones: make(map[int]*github.Milestone),
		repos:      make(map[int]*github.Repository),
	}
}

const timeFormat = "2006-01-02 15:04:05"

func (p *Project) refresh() {
	const perPage = 100
	client := makeClient()
	ctx := context.Background()

	if p.RefreshedAt != (time.Time{}) {
		fmt.Printf("refeshing issues since @ %s\n", p.RefreshedAt.Format(timeFormat))
	} else {
		fmt.Printf("loading issues\n")
	}

	start := time.Now()
	for page := 1; ; {
		issues, resp, err := client.Issues.ListByRepo(
			ctx, p.Owner, p.Repo,
			&github.IssueListByRepoOptions{
				State:     "all",
				Direction: "asc",
				Since:     p.RefreshedAt,
				ListOptions: github.ListOptions{
					Page:    page,
					PerPage: perPage,
				},
			},
		)
		if err != nil {
			log.Print(err)
			time.Sleep(5 * time.Second)
			continue
		}
		if n := len(issues); n > 0 {
			fmt.Printf("  %3d: %d-%d\n", n, *issues[0].Number, *issues[n-1].Number)
		}
		for _, issue := range issues {
			i := p.issues[*issue.Number]
			if i == nil {
				i = &Issue{}
				p.issues[*issue.Number] = i
			}
			i.Issue = *issue
			i.Timeline = nil
			i.Commits = nil
		}

		if resp.NextPage < page {
			break
		}
		page = resp.NextPage
	}

	p.RefreshedAt = start
	p.save()

	fmt.Printf("  done\n")
	fmt.Printf("refreshing timelines\n")

	sorted := p.sortedIssues()
	for j := len(sorted) - 1; j >= 0; j-- {
		num := sorted[j]
		i := p.issues[num]
		changed := false
		if i.PullRequestLinks != nil && i.Commits == nil {
			for page := 1; ; {
				commits, resp, err := client.PullRequests.ListCommits(
					ctx, p.Owner, p.Repo, num,
					&github.ListOptions{
						Page:    page,
						PerPage: perPage,
					},
				)
				if err != nil {
					log.Fatal(err)
				}
				i.Commits = append(i.Commits, commits...)
				changed = true
				if resp.NextPage < page {
					break
				}
				page = resp.NextPage
			}
		}
		if i.Timeline == nil {
			for page := 1; ; {
				timeline, resp, err := client.Issues.ListIssueTimeline(
					ctx, p.Owner, p.Repo, num,
					&github.ListOptions{
						Page:    page,
						PerPage: perPage,
					},
				)
				if err != nil {
					log.Fatal(err)
				}
				i.Timeline = append(i.Timeline, timeline...)
				changed = true
				if resp.NextPage < page {
					break
				}
				page = resp.NextPage
			}
		}
		if changed {
			fmt.Printf("  %d (%d commits, %d events)\n", num, len(i.Commits), len(i.Timeline))
			p.internIssue(i)
			i.save()
		}
	}

	fmt.Printf("  done\n")
}

func (p *Project) sortedIssues() []int {
	n := make([]int, 0, len(p.issues))
	for i := range p.issues {
		n = append(n, i)
	}
	sort.Ints(n)
	return n
}

func (p *Project) internUser(u **github.User) {
	if id := (*u).GetID(); id != 0 {
		if e := p.users[id]; e != nil {
			*u = e
		} else {
			p.users[id] = *u
		}
	}
}

func (p *Project) internMilestone(m **github.Milestone) {
	if id := (*m).GetID(); id != 0 {
		if e := p.milestones[id]; e != nil {
			*m = e
		} else {
			p.milestones[id] = *m
		}
	}
}

func (p *Project) internRepo(r **github.Repository) {
	if id := (*r).GetID(); id != 0 {
		if e := p.repos[id]; e != nil {
			*r = e
		} else {
			p.repos[id] = *r
		}
	}
}

func (p *Project) internIssue(i *Issue) {
	p.internUser(&i.User)
	p.internUser(&i.Assignee)
	p.internUser(&i.ClosedBy)
	for j := range i.Assignees {
		p.internUser(&i.Assignees[j])
	}
	p.internMilestone(&i.Milestone)
	p.internRepo(&i.Repository)

	for _, t := range i.Timeline {
		p.internUser(&t.Actor)
		p.internUser(&t.Assignee)
		p.internMilestone(&t.Milestone)
	}

	for _, c := range i.Commits {
		p.internUser(&c.Author)
		p.internUser(&c.Committer)
	}
}

func (p *Project) load() {
	loadJSON(filepath.Join(*cache, "meta"), p)

	files, err := ioutil.ReadDir(*cache)
	if err != nil {
		log.Fatal(err)
	}
	if len(files) > 0 {
		start := time.Now()
		fmt.Printf("loading %s (%d)\n", *cache, len(files)-1)
		for _, f := range files {
			n, _ := strconv.Atoi(f.Name())
			if n == 0 {
				continue
			}
			i := &Issue{}
			loadJSON(filepath.Join(*cache, f.Name()), i)
			p.internIssue(i)
			p.issues[*i.Number] = i
		}
		fmt.Printf("  done (%d) %.1fs\n", len(p.issues), time.Since(start).Seconds())
	}
}

func (p *Project) save() {
	saveJSON(filepath.Join(*cache, "meta"), p)
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: roachpulse <query>
`)
	flag.PrintDefaults()
	os.Exit(2)
}

func main() {
	flag.Usage = usage
	flag.Parse()
	log.SetFlags(0)
	log.SetPrefix("roachpulse: ")

	if err := os.MkdirAll(*cache, 0755); err != nil {
		log.Fatal(err)
	}

	p := makeProject(*project)
	p.load()
	if *update {
		p.refresh()
	}
	fmt.Printf("\n")

	// TODO:
	// - Mean time to close/merge pull requests.
	// - Mean time to close issues.
	// - Graph on a per weekly basis.
	h := hdrhistogram.New(1, 100*365, 1)
	for _, i := range p.issues {
		if i.PullRequestLinks == nil {
			continue
		}
		if i.ClosedAt == nil {
			continue
		}
		age := i.ClosedAt.Sub(*i.CreatedAt) / (24 * time.Hour)
		if age < 1 {
			age = 1
		}
		h.RecordValue(int64(age))
	}
	fmt.Printf("age: mean=%0.1f stddev=%0.1f\n", h.Mean(), h.StdDev())

	// for _, m := range p.milestones {
	// 	if m.GetState() == "open" {
	// 		fmt.Printf("%s: %d/%d\n", m.GetTitle(), m.GetOpenIssues(), m.GetClosedIssues())
	// 	}
	// }

	// var issues int
	// var pullRequests int
	// for _, i := range p.issues {
	// 	if i.PullRequestLinks == nil {
	// 		issues++
	// 	} else {
	// 		pullRequests++
	// 	}
	// }

	// fmt.Printf("\n")
	// fmt.Printf("%d users\n", len(p.users))
	// fmt.Printf("%d milestones\n", len(p.milestones))
	// fmt.Printf("%d issues\n", issues)
	// fmt.Printf("%d pull-requests\n", pullRequests)

	// metrics:
	// - open issues/PRs
	// - time to respond/close issue/PR
	// - open issue/PR age

	// grouping
	// - by milestone
	// - by user
	// - by week/quarter

	// Total open issues
	// Mean time to close issues
	// Mean time to respond to community reported issues
	// Mean time to respond to community pull requests
	// Percentage of community pull requests that are merged
	// Ratio of issues to merged pull requests
}
