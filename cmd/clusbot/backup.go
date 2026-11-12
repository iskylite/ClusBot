package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"clusbot/internal/work"
)

func runBackup(args []string) error {
	flags := flag.NewFlagSet("clusbot backup", flag.ContinueOnError)
	data := flags.String("data-dir", "./data", "数据目录")
	workspace := flags.String("workspace-root", "", "工作区目录，默认使用数据目录下的 workspace")
	out := flags.String("out", "", "备份目标新目录")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *out == "" {
		return errors.New("使用 backup --data-dir <数据目录> --out <新目录>")
	}
	if *workspace == "" {
		*workspace = filepath.Join(*data, "workspace")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := work.Backup(ctx, *data, *workspace, *out); err != nil {
		return err
	}
	fmt.Println("备份已创建", *out)
	return nil
}

func runRestore(args []string) error {
	flags := flag.NewFlagSet("clusbot restore", flag.ContinueOnError)
	from := flags.String("from", "", "备份目录")
	data := flags.String("data-dir", "", "恢复目标新目录")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *from == "" || *data == "" {
		return errors.New("使用 restore --from <备份目录> --data-dir <新目录>")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := work.Restore(ctx, *from, *data); err != nil {
		return err
	}
	fmt.Println("数据已恢复", *data)
	return nil
}
