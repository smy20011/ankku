package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"syscall"

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

// Repository represents a git repository
type Repository struct {
	repo      *git.Repository
	localDir  string
	remoteURL string
	branch    string
}

// NewRepository creates a repository object from remote url or local directory.
func NewRepository(remoteURL, projectDir, branch string) (res Repository, err error) {
	res.localDir = path.Join(projectDir, "repo")
	res.remoteURL = remoteURL
	res.branch = branch
	if _, err = os.Stat(res.localDir); err == nil {
		repo, err := git.OpenRepository(res.localDir)
		if err != nil {
			return res, err
		}
		res.repo = repo
		return res, nil
	}
	repo, err := git.InitRepository(res.localDir, false)
	if err != nil {
		return res, err
	}
	repo.Remotes.Create("origin", remoteURL)
	res.repo = repo
	return
}

// GitPull performs git pull command on the repository, return true if repo is updated.
func (repo Repository) GitPull() (hasUpdate bool, err error) {
	remote, err := repo.repo.Remotes.Lookup("origin")
	if err != nil {
		return
	}
	err = remote.Fetch([]string{}, nil, "")
	if err != nil {
		return
	}
	remoteBranch, err := repo.repo.References.Lookup("refs/remotes/origin/" + repo.branch)
	if err != nil {
		return
	}
	remoteCommit, err := repo.repo.LookupCommit(remoteBranch.Target())
	if err != nil {
		return
	}
	localBranch, err := repo.repo.LookupBranch(repo.branch, git.BranchLocal)
	if localBranch == nil || err != nil {
		localBranch, err = repo.repo.CreateBranch(repo.branch, remoteCommit, false)
		if err != nil {
			return
		}
		return true, localBranch.SetUpstream("origin/" + repo.branch)
	}
	localCommit, err := repo.repo.LookupCommit(localBranch.Target())
	if err != nil {
		return
	}
	if localCommit.Id().String() != remoteCommit.Id().String() {
		_, err = localBranch.SetTarget(remoteBranch.Target(), "")
		return true, err
	}
	return false, nil
}

// CheckoutToDir check head of branch to specificed directory.
func (repo Repository) CheckoutToDir(dir string) error {
	branch, err := repo.repo.LookupBranch(repo.branch, git.BranchLocal)
	if err != nil {
		return err
	}
	commit, err := repo.repo.LookupCommit(branch.Target())
	if err != nil {
		return err
	}
	tree, err := commit.Tree()
	if err != nil {
		return err
	}
	return repo.repo.CheckoutTree(tree, &git.CheckoutOpts{
		Strategy:        git.CheckoutForce,
		TargetDirectory: dir,
	})
}

type Application struct {
	repository     Repository
	environmentDir string
	applicationDir string
	commandChannel chan *exec.Cmd
	stopChannel    chan chan bool
	port           int
}

// NewApplication creates a new app.
func NewApplication(repository Repository, projectDir string, port int) Application {
	return Application{
		repository, path.Join(projectDir, "env"),
		path.Join(projectDir, "app"),
		make(chan *exec.Cmd),
		make(chan chan bool),
		port,
	}
}

// Reload start the "web" application defined in the Procfile and
// restart the app if already started.
func (app Application) reloadImpl() error {
	err := app.repository.CheckoutToDir(app.applicationDir)
	if err != nil {
		return err
	}
	err = app.setupVirtualEnv()
	if err != nil {
		return err
	}
	cmd, err := app.findProcCommand("web")
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
	script := fmt.Sprintf(scriptTmpl, app.environmentDir, app.applicationDir, app.port, cmd)
	log.Printf("Starting server command: %v", cmd)
	command := exec.Command("bash", "-c", script)
	app.commandChannel <- command
	return nil
}

func setupCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = os.Stdout
}

func mayKillCommand(command *exec.Cmd) {
	if command != nil && command.Process != nil {
		log.Printf("Killing existing process %v", command.Process)
		pgid, _ := syscall.Getpgid(command.Process.Pid)
		syscall.Kill(-pgid, syscall.SIGKILL)
	}

}

// StartDaemon start the monitor process of the server.
func (app Application) StartDaemon() {
	go func() {
		var command *exec.Cmd
		var done chan error
		timeFailed := 0
		for {
			select {
			case newCommand := <-app.commandChannel:
				{
					mayKillCommand(command)
					done = make(chan error)
					timeFailed = 0
					command = newCommand
				}
			case err := <-done:
				{
					switch err.(type) {
					case *exec.ExitError:
						if err.(*exec.ExitError).ExitCode() != 0 && timeFailed < 3 {
							timeFailed++
							log.Printf("Process failed to start %d times, starting.. %v", timeFailed, err)
							command = exec.Command(command.Path, command.Args...)
						}
					default:
						log.Fatalf("Failed to wait for process: %v", err.Error())
					}
				}
			case out := <-app.stopChannel:
				log.Println("Process stopped, close all subprocesses!")
				mayKillCommand(command)
				out <- true
				return
			}
			setupCommand(command)
			command.Start()
			go func(done chan error, command *exec.Cmd) {
				done <- command.Wait()
			}(done, command)
		}
	}()
}

func (app Application) setupVirtualEnv() error {
	if _, err := os.Stat(app.environmentDir); os.IsNotExist(err) {
		cmd := exec.Command("virtualenv", app.environmentDir)
		cmd.Stdout = os.Stdout
		err = cmd.Run()
		if err != nil {
			output, _ := cmd.Output()
			return fmt.Errorf("Error while create virtual env: %s", string(output))
		}
	}
	return nil
}

func (app Application) findProcCommand(commandName string) (string, error) {
	procFile := path.Join(app.applicationDir, "Procfile")
	reader, err := ioutil.ReadFile(procFile)
	commands := make(map[string]string)
	err = yaml.Unmarshal(reader, &commands)
	if err != nil {
		return "", err
	}
	cmd, exists := commands[commandName]
	if !exists {
		return "", fmt.Errorf("Cannot find command named %s", commandName)
	}
	return cmd, nil
}

// Reload start the "web" application defined in the Procfile and
// restart the app if already started. By default, Reload will only
// restart server when it's updated, use force to force restart.
func (app Application) Reload(force bool) error {
	updated, err := app.repository.GitPull()
	log.Printf("update: %v", updated)
	if err != nil {
		return err
	}
	if updated || force {
		log.Printf("Reload application")
		return app.reloadImpl()
	}
	return nil
}

func (app Application) Stop() {
	result := make(chan bool)
	app.stopChannel <- result
	<-result
}

func main() {
	flag.Parse()
	repository, err := NewRepository(*gitRepo, *projectDir, *branch)
	if err != nil {
		panic(err)
	}

	application := NewApplication(repository, *projectDir, *port)
	application.StartDaemon()
	err = application.Reload(true)
	if err != nil {
		panic(err)
	}

	c := cron.New()
	c.AddFunc("@every 5s", func() {
		err := application.Reload(false)
		if err != nil {
			panic(err)
		}
	})
	c.Start()

	killed := make(chan os.Signal, 2)
	signal.Notify(killed, os.Interrupt, os.Kill)
	<-killed
	log.Println("Killed")
	application.Stop()
}
