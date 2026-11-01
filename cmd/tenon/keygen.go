package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"tenon.dev/internal/protocol"
)

// runKeygen 生成宿主签名密钥对：私钥写 PKCS#8 PEM，公钥写模块信任集格式
// （kid 到 base64 公钥的映射，可直接放进 TENON_TRUSTED_KEYS 指向的文件）。
func runKeygen(args []string) (int, error) {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	out := fs.String("out", "", "私钥输出路径（PKCS#8 PEM）")
	pubPath := fs.String("pub", "", "公钥输出路径（信任集 JSON）")
	force := fs.Bool("force", false, "允许覆盖已存在的文件")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if *out == "" || *pubPath == "" {
		return 2, fmt.Errorf("用法：tenon keygen --out <私钥路径> --pub <公钥路径> [--force]")
	}
	if !*force {
		for _, p := range []string{*out, *pubPath} {
			if _, err := os.Stat(p); err == nil {
				return 2, fmt.Errorf("%s 已存在；覆盖请加 --force", p)
			}
		}
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return 1, fmt.Errorf("生成密钥：%w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return 1, fmt.Errorf("编码私钥：%w", err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	if dir := filepath.Dir(*out); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return 1, fmt.Errorf("创建私钥目录：%w", err)
		}
	}
	// 先写私钥再写公钥：公钥在而私钥丢失，是最容易被误判成「能签」的状态。
	if err := os.WriteFile(*out, block, 0o600); err != nil {
		return 1, fmt.Errorf("写私钥 %s：%w", *out, err)
	}
	kid := protocol.KeyID(pub)
	set := map[string]string{kid: base64.StdEncoding.EncodeToString(pub)}
	body, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return 1, fmt.Errorf("编码公钥：%w", err)
	}
	if err := os.WriteFile(*pubPath, append(body, byte(10)), 0o644); err != nil {
		return 1, fmt.Errorf("写公钥 %s：%w", *pubPath, err)
	}

	// Windows 上 os.WriteFile 的权限位不生效，必须显式收紧 ACL，否则等于没说。
	if runtime.GOOS == "windows" {
		fmt.Println("注意：Windows 不认 Unix 权限位，私钥当前继承目录 ACL。收紧办法：")
		fmt.Println("icacls " + strconv.Quote(*out) + " /inheritance:r /grant:r %USERNAME%:R")
	}
	fmt.Println("kid  " + kid)
	fmt.Println("私钥 " + *out + "（不要入库）")
	fmt.Println("公钥 " + *pubPath + "（可合并进 TENON_TRUSTED_KEYS）")
	return 0, nil
}
