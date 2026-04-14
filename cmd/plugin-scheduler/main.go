package main

import (
	"os"

	"k8s.io/component-base/cli"
	"k8s.io/kubernetes/cmd/kube-scheduler/app"

	"github.com/ezebunandu/k8s-alpha-scheduler/pkg/alphabeticalscore"
)

func main() {
	command := app.NewSchedulerCommand(
		app.WithPlugin(alphabeticalscore.Name, alphabeticalscore.New),
	)
	code := cli.Run(command)
	os.Exit(code)
}
