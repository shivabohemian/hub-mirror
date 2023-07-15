package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"text/template"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/spf13/pflag"
)

var (
	content    = pflag.StringP("content", "", "", "原始镜像，格式为：{ \"hub-mirror\": [] }")
	maxContent = pflag.IntP("maxContent", "", 10, "原始镜像个数限制")
	username   = pflag.StringP("username", "", "", "docker hub 用户名")
	password   = pflag.StringP("password", "", "", "docker hub 密码")
	outputPath = pflag.StringP("outputPath", "", "output.sh", "结果输出路径")
	repository = pflag.StringP("repository", "", "", "仓库地址,如果为空,默认推到dockerHub")
)

func main() {
	pflag.Parse()

	fmt.Println("验证原始镜像内容")
	var hubMirrors struct {
		Content []string `json:"hub-mirror"`
	}
	err := json.Unmarshal([]byte(*content), &hubMirrors)
	if err != nil {
		panic(err)
	}
	if len(hubMirrors.Content) > *maxContent {
		panic("content is too long.")
	}
	fmt.Printf("%+v\n", hubMirrors)

	fmt.Println("连接 Docker")
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		panic(err)
	}

	fmt.Println("验证 Docker 用户名密码")
	if *username == "" || *password == "" {
		panic("username or password cannot be empty.")
	}
	authConfig := types.AuthConfig{
		Username:      *username,
		Password:      *password,
		ServerAddress: *repository,
	}
	encodedJSON, err := json.Marshal(authConfig)
	if err != nil {
		panic(err)
	}
	authStr := base64.URLEncoding.EncodeToString(encodedJSON)
	_, err = cli.RegistryLogin(context.Background(), authConfig)
	if err != nil {
		panic(err)
	}

	fmt.Println("开始转换镜像")
	output := make([]struct {
		Source     string
		Target     string
		Repository string
	}, 0)

	wg := sync.WaitGroup{}

	for _, source := range hubMirrors.Content {
		if source == "" {
			continue
		}

		target := source
		// 查看是否配置自定义镜像名，如果配置的话使用自定义镜像名
		if strings.Contains(source, "$") {
			str1 := strings.Split(source, "$")
			repository := strings.Split(str1[0], ":")
			target = str1[1] + ":" + repository[len(repository)-1]
			source = str1[0]
		}

		// 如果为空,默认推送到 DockerHub 用户名 下
		// 如果指定了值,则推动到指定的仓库下,用户名不一定与repository后缀相同
		if *repository == "" {
			target = *username + "/" + strings.ReplaceAll(target, "/", ".")
		} else {
			target = *repository + "/" + strings.ReplaceAll(target, "/", ".")
		}

		wg.Add(1)
		go func(source, target, repository string) {
			defer wg.Done()

			fmt.Println("开始转换", source, "=>", target)

			// 获取镜像清单信息并提取 manifest 条目
			res, _, err := RunCmdWithRes(exec.Command("docker", "manifest", "inspect", source))
			if err != nil {
				return
			}
			var manifestInspect ManifestInspect
			err = json.Unmarshal([]byte(res), &manifestInspect)
			if err != nil {
				panic(err)
			}

			// 遍历每个 manifest 条目并同步到目标仓库
			ctx := context.Background()
			var pullOut, pushOut io.ReadCloser
			defer func() {
				if pullOut != nil {
					pullOut.Close()
				}
				if pushOut != nil {
					pushOut.Close()
				}
			}()
			for i := range manifestInspect.Manifests {
				// 拉取镜像
				pullOut, err = cli.ImagePull(ctx, source+"@"+manifestInspect.Manifests[i].Digest,
					types.ImagePullOptions{})
				if err != nil {
					panic(err)
				}
				io.Copy(os.Stdout, pullOut)

				// 重新标签
				err = cli.ImageTag(ctx, source, target)
				if err != nil {
					panic(err)
				}

				// 上传镜像
				pushOut, err = cli.ImagePush(ctx, target, types.ImagePushOptions{
					RegistryAuth: authStr,
				})
				if err != nil {
					panic(err)
				}
				io.Copy(os.Stdout, pushOut)
			}
			output = append(output, struct {
				Source     string
				Target     string
				Repository string
			}{Source: source, Target: target, Repository: repository})
			fmt.Println("转换成功", source, "=>", target)
		}(source, target, *repository)
	}

	wg.Wait()

	if len(output) == 0 {
		panic("output is empty.")
	}

	tmpl, err := template.New("pull_images").Parse(`{{- range . -}}
	
{{if .Repository}}
# if your repository is private,please login...
# docker login {{ .Repository }} --username={your username}
{{end}}	
docker pull {{ .Target }}
docker tag {{ .Target }} {{ .Source }}

{{ end -}}`)
	if err != nil {
		panic(err)
	}
	outputFile, err := os.Create(*outputPath)
	if err != nil {
		panic(err)
	}
	defer outputFile.Close()
	err = tmpl.Execute(outputFile, output)
	if err != nil {
		panic(err)
	}
	fmt.Println(output)
}

type ManifestInspect struct {
	SchemaVersion int        `json:"schemaVersion"`
	MediaType     string     `json:"mediaType"`
	Manifests     []Manifest `json:"manifests"`
}

type Manifest struct {
	MediaType string    `json:"mediaType"`
	Size      int64     `json:"size"`
	Digest    string    `json:"digest"`
	Platform  *Platform `json:"platform"`
}

type Platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Version      string `json:"os.version"`
	Variant      string `json:"variant"`
}

func RunCmdWithRes(cmd *exec.Cmd) (string, string, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	outStr, errStr := string(stdout.Bytes()), string(stderr.Bytes())
	return outStr, errStr, err
}
