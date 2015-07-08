package main

import (
	"github.com/codegangsta/cli"
	"os"
	"pfi/sensorbee/sensorbee/cmd/lib/run"
	"pfi/sensorbee/sensorbee/cmd/lib/shell"
	"pfi/sensorbee/sensorbee/cmd/lib/topology"
	"time"
)

func init() {
	// TODO
	time.Local = time.UTC
}

func main() {
	app := cli.NewApp()
	app.Name = "sensorbee"
	app.Usage = "SenserBee"
	app.Version = "0.0.1" // TODO get dynamic, will be get from external file
	app.Commands = []cli.Command{
		run.SetUp(),
		shell.SetUp(),
		topology.SetUp(),
	}
	if err := app.Run(os.Args); err != nil {
		os.Exit(1)
	}
}
