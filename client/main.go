package client

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/kilgaloon/leprechaun/config"
	"github.com/kilgaloon/leprechaun/context"
	"github.com/kilgaloon/leprechaun/event"
	"github.com/kilgaloon/leprechaun/log"
	"github.com/kilgaloon/leprechaun/api"
	"github.com/kilgaloon/leprechaun/workers"
)

// Agent holds instance of Client
var Agent *Client

// Client settings and configurations
type Client struct {
	PID    int
	Config *config.ClientConfig
	Logs   log.Logs
	Queue
	Context *context.Context
	mu      *sync.Mutex
	Workers *workers.Workers
}

// CreateAgent new client
// Creating new agent will enable usage of Agent variable globally for packages
// that use this package
func CreateAgent(cfg *config.ClientConfig) *Client {
	client := &Client{}
	// load configurations for server
	client.Config = cfg
	// be carefull BuildContext might use Config, so set it before us it
	client.Context = context.BuildContext(client)
	client.mu = new(sync.Mutex)
	client.Logs = log.Logs{
		ErrorLog: client.Config.ErrorLog,
		InfoLog:  client.Config.InfoLog,
	}
	client.Workers = workers.BuildWorkers(client.Context, cfg.MaxAllowedWorkers, client.Logs)

	Agent = client

	return Agent
}

// Start client
func (client *Client) Start() {
	// remove hanging .lock file
	os.Remove(client.Config.LockFile)
	// SetPID of client
	client.SetPID()
	// build queue
	client.BuildQueue()

	// watch for new recipes
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		panic("Failed to create watcher")
	}

	defer watcher.Close()
	go func() {
		for {
			select {
			case event := <-watcher.Events:
				if event.Op&fsnotify.Create == fsnotify.Create {
					client.AddToQueue(&client.Queue.Stack, event.Name)
				}
			case err := <-watcher.Errors:
				client.Logs.Error("error:", err)
			}
		}
	}()

	err = watcher.Add(client.Config.RecipesPath)
	if err != nil {
		fmt.Println(err)
	}

	event.EventHandler.Dispatch("client:ready")
	// register client to command socket
	go api.BuildSocket(client.Config.CommandSocket).Register(client)

	for {
		go client.ProcessQueue()
		time.Sleep(60 * time.Second)
	}

}

// SetPID sets current PID of client
func (client *Client) SetPID() {
	f, err := os.OpenFile(client.Config.PIDFile, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		panic("Failed to start client, can't save PID")
	}

	client.PID = os.Getpid()
	pid := strconv.Itoa(client.PID)
	_, err = f.WriteString(pid)
	if err != nil {
		panic("Failed to start client, can't save PID")
	}
}

// GetPID gets current PID of client
func (client Client) GetPID() int {
	return client.PID
}

// Check does client is working on something
// decide this in which status client resides
func (client Client) isWorking() bool {
	// check does .lock file exists
	if _, err := os.Stat(client.Config.LockFile); os.IsNotExist(err) {
		return false
	}

	return true
}

// Lock client to busy state
func (client Client) Lock() {
	_, err := os.OpenFile(client.Config.LockFile, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		panic("Failed to lock client in busy state")
	}
}

// Unlock client to busy state
func (client *Client) Unlock() {
	os.Remove(client.Config.LockFile)
}

// Stop client
func (client Client) Stop() os.Signal {
	var answer string
	forceQuit := false
	quit := false

	fmt.Print("Are you sure?(y/N): ")
	fmt.Scanf("%s", &answer)

	if client.isWorking() && strings.ToUpper(answer) == "Y" {
		answer = ""
		// if user doesn't choose to force quit we will wait for process, otherwise KILL IT
		fmt.Print("Client is working on something in the background. Force quit? (y/N)")
		fmt.Scanf("%s", &answer)

		if strings.ToUpper(answer) == "Y" {
			forceQuit = true
		}
	} else if !client.isWorking() && strings.ToUpper(answer) == "Y" {
		quit = true
	}

	pid := client.GetPID()
	process, err := os.FindProcess(pid)
	if err != nil {
		client.Logs.Error("Can't find process with that PID. %s", err)
	}

	// shutdown gracefully
	if quit {
		state, err := process.Wait()
		client.Logs.Info("Stopping Leprechaun, please wait...")

		if err == nil {
			if state.Exited() {
				client.Unlock()
				return syscall.SIGTERM
			}
		} else {
			forceQuit = true
		}
	}

	// force quite and terminate everything
	if forceQuit {
		killed := process.Kill()
		if killed != nil {
			client.Logs.Error("Can't kill process with that PID. %s", killed)
		} else {
			client.Unlock()
			return syscall.SIGTERM
		}
	}

	return os.Interrupt
}

// GetConfig Gets config for server
func (client Client) GetConfig() *config.ClientConfig {
	return client.Config
}

func init() {
	// subscribe to events for this package
	event.EventHandler.Subscribe("client:lock", func() {
		Agent.Lock()
	})

	event.EventHandler.Subscribe("client:unlock", func() {
		Agent.Unlock()
	})
}
