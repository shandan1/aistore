// Package commands provides the set of CLI commands used to communicate with the AIS cluster.
// This specific file handles the CLI commands that interact with cluster xactions
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package commands

import (
	"fmt"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cli/templates"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/urfave/cli"
)

const (
	xactStart = cmn.ActXactStart
	xactStop  = cmn.ActXactStop
	xactStats = cmn.ActXactStats
)

var (
	baseXactFlag = []cli.Flag{bucketFlag}

	xactFlags = map[string][]cli.Flag{
		xactStart: baseXactFlag,
		xactStop:  baseXactFlag,
		xactStats: {jsonFlag},
	}

	xactGeneric    = "%s xaction %s --xact <value>"
	xactStartUsage = fmt.Sprintf(xactGeneric, cliName, xactStart)
	xactStopUsage  = fmt.Sprintf(xactGeneric, cliName, xactStop)
	xactStatsUsage = fmt.Sprintf(xactGeneric, cliName, xactStats)

	XactCmds = []cli.Command{
		{
			Name:  cmn.GetWhatXaction,
			Usage: "command that interacts with extended actions (xactions)",
			Flags: baseXactFlag,
			Subcommands: []cli.Command{
				{
					Name:         xactStart,
					Usage:        "starts the extended action",
					UsageText:    xactStartUsage,
					Flags:        xactFlags[xactStart],
					Action:       xactHandler,
					BashComplete: xactList,
				},
				{
					Name:         xactStop,
					Usage:        "stops the extended action",
					UsageText:    xactStopUsage,
					Flags:        xactFlags[xactStop],
					Action:       xactHandler,
					BashComplete: xactList,
				},
				{
					Name:         xactStats,
					Usage:        "returns the stats of the extended action",
					UsageText:    xactStatsUsage,
					Flags:        xactFlags[xactStats],
					Action:       xactHandler,
					BashComplete: xactList,
				},
			},
		},
	}
)

func xactHandler(c *cli.Context) (err error) {
	var (
		baseParams = cliAPIParams(ClusterURL)
		command    = c.Command.Name
		bucket     = parseFlag(c, bucketFlag.Name)
		xaction    = c.Args().First()
	)

	_, ok := cmn.ValidXact(xaction)

	if !ok && xaction != "" {
		return fmt.Errorf("%q is not a valid xaction", xaction)
	}

	xactStatsMap, err := api.GetXactionResponse(baseParams, xaction, command, bucket)
	if err != nil {
		return errorHandler(err)
	}

	switch command {
	case xactStart:
		if xaction == "" {
			fmt.Println("started all xactions")
		} else {
			fmt.Printf("started %q xaction\n", xaction)
		}
	case xactStop:
		if xaction == "" {
			fmt.Println("stopped all xactions")
		} else {
			fmt.Printf("stopped %q xaction\n", xaction)
		}
	case xactStats:
		err = templates.DisplayOutput(xactStatsMap, templates.XactStatsTmpl, flagIsSet(c, jsonFlag.Name))
	default:
		return fmt.Errorf(invalidCmdMsg, command)
	}
	return errorHandler(err)
}