package main

import (
	"context"
	"fmt"
	"os"

	"github.com/ipfs/go-cid"
	golog "github.com/ipfs/go-log"
	wnfs "github.com/qri-io/wnfs-go"
	wnipfs "github.com/qri-io/wnfs-go/ipfs"
	"github.com/qri-io/wnfs-go/mdstore"
	cli "github.com/urfave/cli/v2"
)

func open(ctx context.Context) (wnfs.WNFS, *ExternalState) {
	store, err := wnipfs.NewFilesystem(ctx, map[string]interface{}{
		"path": os.Getenv("IPFS_PATH"),
	})

	if err != nil {
		errExit("error: opening IPFS repo: %s\n", err)
	}

	statePath, err := ExternalStatePath()
	if err != nil {
		errExit("error: getting state path: %s\n", err)
	}
	state, err := LoadOrCreateExternalState(statePath)
	if err != nil {
		errExit("error: loading external state: %s\n", err)
	}

	var fs wnfs.WNFS
	if state.RootCID.Equals(cid.Cid{}) {
		fmt.Printf("creating new wnfs filesystem...")
		if fs, err = wnfs.NewEmptyFS(ctx, store); err != nil {
			errExit("error: creating empty WNFS: %s\n", err)
		}
		fmt.Println("done")
	} else {
		if fs, err = wnfs.FromCID(ctx, store, state.RootCID); err != nil {
			errExit("error: opening WNFS CID %s: %s\n", state.RootCID, err.Error())
		}
	}

	return fs, state
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fs, state := open(ctx)

	updateExternalState := func() {
		state.RootCID = fs.(mdstore.DagNode).Cid()
		fmt.Printf("writing root cid: %s...", state.RootCID)
		if err := state.Write(); err != nil {
			errExit("error: writing external state: %s\n", err)
		}
		fmt.Println("done")
	}

	app := &cli.App{
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:    "verbose",
				Aliases: []string{"v"},
				Usage:   "print verbose output",
			},
		},
		Before: func(c *cli.Context) error {
			if c.Bool("verbose") {
				golog.SetLogLevel("wnfs", "debug")
			}
			return nil
		},
		Commands: []*cli.Command{
			{
				Name:  "mkdir",
				Usage: "create a directory",
				Action: func(c *cli.Context) error {
					defer updateExternalState()
					return fs.Mkdir(c.Args().Get(0), wnfs.MutationOptions{
						Commit: true,
					})
				},
			},
			{
				Name:  "cat",
				Usage: "cat a file",
				Action: func(c *cli.Context) error {
					data, err := fs.Cat(c.Args().Get(0))
					if err != nil {
						return err
					}
					_, err = os.Stdout.Write(data)
					return err
				},
			},
			{
				Name:    "write",
				Aliases: []string{"add"},
				Usage:   "add a file to wnfs",
				Action: func(c *cli.Context) error {
					path := c.Args().Get(0)
					file := c.Args().Get(1)
					f, err := os.Open(file)
					if err != nil {
						return err
					}

					defer updateExternalState()
					return fs.Write(path, f, wnfs.MutationOptions{
						Commit: true,
					})
				},
			},
			{
				Name:  "ls",
				Usage: "list the contents of a directory",
				Action: func(c *cli.Context) error {
					entries, err := fs.Ls(c.Args().Get(0))
					if err != nil {
						return err
					}

					for _, entry := range entries {
						fmt.Println(entry.Name())
					}
					return nil
				},
			},
			{
				Name:  "rm",
				Usage: "remove files and directories",
				Action: func(c *cli.Context) error {
					defer updateExternalState()
					return fs.Rm(c.Args().Get(0), wnfs.MutationOptions{
						Commit: true,
					})
				},
			},
			{
				Name:  "tree",
				Usage: "show a tree rooted at a given path",
				Action: func(c *cli.Context) error {
					path := c.Args().Get(0)
					// TODO(b5): can't yet create tree from wnfs root.
					// for now replace empty string with "public"
					if path == "" {
						path = "public"
					}

					s, err := treeString(fs, path)
					if err != nil {
						return err
					}

					os.Stdout.Write([]byte(s))
					return nil
				},
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		errExit(err.Error())
	}
}

func errExit(msg string, v ...interface{}) {
	fmt.Printf(msg, v...)
	os.Exit(1)
}