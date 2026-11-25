package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"clusbot/internal/work"
	"gopkg.in/yaml.v3"
)

func runImportDomainAssets(args []string) error {
	flags := flag.NewFlagSet("import-domain-assets", flag.ContinueOnError)
	directory := flags.String("data-dir", "./data", "ClusBot 数据目录")
	file := flags.String("file", "", "本地资产清单 YAML")
	var account string
	var selectedAccount bool
	flags.Func("account", "已初始化的目标账号登录名（仅限离线数据目录操作者）", func(username string) error {
		if selectedAccount {
			return errors.New("--account 只能指定一次")
		}
		selectedAccount = true
		account = strings.TrimSpace(username)
		return nil
	})
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *file == "" || account == "" || len(account) > 64 || flags.NArg() != 0 {
		return errors.New("使用 import-domain-assets --account <账号登录名> --file <资产清单> --data-dir <数据目录>")
	}
	source, err := os.Open(*file)
	if err != nil {
		return err
	}
	defer source.Close()
	const manifestLimit = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(source, manifestLimit+1))
	if err != nil {
		return err
	}
	if len(raw) > manifestLimit {
		return errors.New("资产清单超过 1 MiB")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var manifest struct {
		Assets []work.DomainAssetEntry `yaml:"assets"`
	}
	if err := decoder.Decode(&manifest); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("资产清单须为一个 YAML 文档")
	}
	// A typed pointer decodes explicit YAML null the same as an omitted
	// field. Inspect only start nodes so blanks/null never enable a grant
	// immediately by falling back to legacy behavior.
	var windows struct {
		Assets []struct {
			Benchmark []struct {
				Start yaml.Node `yaml:"valid_from_unix_ms"`
			} `yaml:"benchmark_grants"`
		} `yaml:"assets"`
	}
	if err := yaml.Unmarshal(raw, &windows); err != nil {
		return err
	}
	for _, asset := range windows.Assets {
		for _, grant := range asset.Benchmark {
			if !grant.Start.IsZero() && grant.Start.ShortTag() != "!!int" {
				return errors.New("基准授权 valid_from_unix_ms 必须是 UNIX 毫秒整数，留空请删除该字段")
			}
		}
	}
	dir, err := filepath.EvalSymlinks(*directory)
	if err != nil {
		return err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	lock, err := work.LockDataDir(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	ctx := context.Background()
	instancePath := filepath.Join(dir, "clusbot.db")
	info, err := os.Lstat(instancePath)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("没有已初始化的 ClusBot 账号实例，不能导入")
	}
	if err := requireAccountInstance(ctx, instancePath); err != nil {
		return err
	}
	instance, err := work.Open(instancePath)
	if err != nil {
		return err
	}
	defer instance.Close()
	// The stopped instance is the authority for the active login name. No
	// password or password hash is needed or displayed by this offline tool.
	principal, _, err := instance.AccountPassword(ctx, account)
	if errors.Is(err, work.ErrAccountDenied) {
		return errors.New("指定账号不存在或未启用，不能导入")
	}
	if err != nil {
		return err
	}
	key, ok := strings.CutPrefix(principal, "user:")
	if !ok || !validAccountDirectory(key) {
		return errors.New("指定账号标识无效，不能导入")
	}
	accountRoot := filepath.Join(dir, "account-data")
	privateDir := filepath.Join(accountRoot, key)
	for _, name := range []string{accountRoot, privateDir} {
		if fi, err := os.Lstat(name); err != nil || !fi.IsDir() {
			return errors.New("账号个人数据目录未初始化或无效；请先登录并打开个人助手")
		}
	}
	path := filepath.Join(privateDir, "clusbot.db")
	if err := verifyAssetImportOwner(ctx, path, principal); err != nil {
		return err
	}
	store, err := work.OpenAccountBusinessStore(path, instance)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.ImportDomainAssetsForAccount(ctx, principal, manifest.Assets); err != nil {
		return err
	}
	fmt.Printf("已向账号 %s 导入 %d 条资产配置\n", account, len(manifest.Assets))
	return nil
}

func verifyAssetImportOwner(ctx context.Context, path, principal string) error {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return errors.New("账号个人数据库未初始化或无效；请先登录并打开个人助手")
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	defer db.Close()
	var owner string
	if err := db.QueryRowContext(ctx, "SELECT principal_id FROM account_business_owner WHERE singleton=1").Scan(&owner); err != nil || owner != principal {
		return errors.New("账号数据库的归属无法确认；不会修改或接管该数据库")
	}
	return nil
}
