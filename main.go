package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"

	yaml "gopkg.in/yaml.v2"

	cron "github.com/robfig/cron"
	git "gopkg.in/libgit2/git2go.v26"
)

var (
	gitRepo    = flag.String("git_repo", "", "Remote git repo to monitor.")
	projectDir = flag.String("project_dir", ".", "Path to store all project related files")
	branch     = flag.String("branch_name", "master", "Git branch to monitor, default master.")
	port       = flag.Int("port", 5005, "Port for server to listen on")
	serverProc *exec.Cmd
)

// Clone or create initial repository.
func cloneOrOpenRepo(url string) (*git.Repository, error) {
	repoDir := path.Join(*projectDir, "repo")
	if info, err := os.Stat(repoDir); err != nil && info != nil {
		return git.OpenRepository(repoDir)
	}
	repo, err := git.InitRepository(repoDir, false)
	if err != nil {
		return nil, err
	}
	repo.Remotes.Create("origin", url)
	return repo, err
}

// Perform git pull command on the repository, return true if repo is updated.
func gitPull(repo *git.Repository, branch string) (hasUpdate bool, err error) {
	remote, err := repo.Remotes.Lookup("origin")
	if err != nil {
		return
	}
	err = remote.Fetch([]string{}, nil, "")
	if err != nil {
		return
	}
	remoteBranch, err := repo.References.Lookup("refs/remotes/origin/" + branch)
	if err != nil {
		return
	}
	remoteCommit, err := repo.LookupCommit(remoteBranch.Target())
	if err != nil {
		return
	}
	localBranch, err := repo.LookupBranch(branch, git.BranchLocal)
	if localBranch == nil || err != nil {
		localBranch, err = repo.CreateBranch(branch, remoteCommit, false)
		if err != nil {
			return
		}
		return true, localBranch.SetUpstream("origin/" + branch)
	}
	localCommit, err := repo.LookupCommit(localBranch.Target())
	if err != nil {
		return
	}
	if localCommit.Id().String() != remoteCommit.Id().String() {
		_, err = localBranch.SetTarget(remoteBranch.Target(), "")
		return true, err
	}
	return false, nil
}

func startApp(repo *git.Repository) error {
	appDir := path.Join(*projectDir, "app")
	err := os.RemoveAll(appDir)
	if err != nil {
		return err
	}
	branch, err := repo.LookupBranch(*branch, git.BranchLocal)
	if err != nil {
		return err
	}
	commit, err := repo.LookupCommit(branch.Target())
	if err != nil {
		return err
	}
	tree, err := commit.Tree()
	if err != nil {
		return err
	}
	err = repo.CheckoutTree(tree, &git.CheckoutOpts{
		Strategy:        git.CheckoutForce,
		TargetDirectory: appDir,
	})
	if err != nil {
		return err
	}
	envDir := path.Join(*projectDir, "venv")
	if _, err := os.Stat(envDir); os.IsNotExist(err) {
		cmd := exec.Command("virtualenv", envDir)
		pipeOutputs(cmd)
		err = cmd.Run()
		if err != nil {
			output, _ := cmd.Output()
			return fmt.Errorf("Error while create virtual env: %s", string(output))
		}
	}

	procFile := path.Join(*projectDir, "app", "Procfile")
	reader, err := ioutil.ReadFile(procFile)
	commands := make(map[string]string)
	err = yaml.Unmarshal(reader, &commands)
	if err != nil {
		return err
	}
	scriptTmpl := `
	source %s/bin/activate
	cd %s
	pip install -r requirements.txt
	export PORT=%d
	%s
	`
	script := fmt.Sprintf(scriptTmpl, envDir, appDir, *port, commands["web"])
	serverProc = exec.Command("bash", "-c", script)
	pipeOutputs(serverProc)
	return serverProc.Start()
}

func pipeOutputs(cmd *exec.Cmd) {
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
}

func pullAndDeploy(repo *git.Repository) error {
	updated, err := gitPull(repo, *branch)
	if err != nil {
		panic(err)
	}
	if updated || serverProc == nil {
		return startApp(repo)
	} else if updated {
		err = serverProc.Process.Kill()
		if err != nil {
			return err
		}
		startApp(repo)
	}
	return nil
}

func main() {
	flag.Parse()
	repo, err := cloneOrOpenRepo(*gitRepo)
	if err != nil {
		panic(err)
	}
	pullAndDeploy(repo)
	c := cron.New()
	c.AddFunc("@every 5s", func() {
		err := pullAndDeploy(repo)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to execute application: %v", err)
		}
	})
	c.Start()
	// Block forever
	select {}
}
