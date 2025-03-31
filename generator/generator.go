/*
This program generates the protobuf and SteamLanguage files from the SteamKit data.
*/
package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var printCommands = false

// Generator 接口定义了所有生成器的共同行为
type Generator interface {
	Run() error
}

// BaseGenerator 包含生成器的通用功能
type BaseGenerator struct {
	OutputDir string
}

// SteamLanguageGenerator 用于生成SteamLanguage文件
type SteamLanguageGenerator struct {
	BaseGenerator
	SteamKitPath string
}

// ProtobufGenerator 用于生成Protobuf文件
type ProtobufGenerator struct {
	BaseGenerator
	SourceBase   string
	SourceSubdir string
	ProtoFile    string
	TargetFile   string
	GoOpts       []string
}

func main() {
	args := strings.Join(os.Args[1:], " ")

	found := false
	if strings.Contains(args, "clean") {
		clean()
		found = true
	}
	if strings.Contains(args, "steamlang") {
		buildSteamLanguage()
		found = true
	}
	if strings.Contains(args, "proto") {
		buildProto()
		found = true
	}

	if !found {
		_, _ = fmt.Fprintln(os.Stderr, "Invalid target!\nAvailable targets: clean, proto, steamlang")
		os.Exit(1)
	}
}

func clean() {
	print("# Cleaning")
	cleanGlob("../protocol/**/**/*.pb.go")

	_ = os.Remove("../protocol/steamlang/enums.go")
	_ = os.Remove("../protocol/steamlang/messages.go")
}

func cleanGlob(pattern string) {
	protos, _ := filepath.Glob(pattern)
	for _, proto := range protos {
		err := os.Remove(proto)
		if err != nil {
			panic(err)
		}
	}
}

func (g *SteamLanguageGenerator) Run() error {
	print("# Building Steam Language")

	// 确保输出目录存在
	err := os.MkdirAll(g.OutputDir, os.ModePerm)
	if err != nil {
		return err
	}

	err = execute("dotnet", []string{
		"run",
		"-c", "release",
		"-p", "./GoSteamLanguageGenerator",
		g.SteamKitPath,
		g.OutputDir,
	})
	if err != nil {
		return err
	}

	// 格式化生成的文件
	return execute("gofmt", []string{
		"-w",
		filepath.Join(g.OutputDir, "enums.go"),
		filepath.Join(g.OutputDir, "messages.go"),
	})
}

func buildSteamLanguage() {
	generator := &SteamLanguageGenerator{
		BaseGenerator: BaseGenerator{
			OutputDir: "../protocol/steamlang",
		},
		SteamKitPath: "./SteamKit",
	}

	err := generator.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error building Steam Language:", err)
		os.Exit(1)
	}
}

func (g *ProtobufGenerator) Run() error {
	outDir, _ := filepath.Split(g.TargetFile)
	err := os.MkdirAll(outDir, os.ModePerm)
	if err != nil {
		return err
	}

	// 构建protoc命令参数
	args := []string{
		"-I=" + g.SourceBase,
		"-I=" + g.SourceBase + "/google",
		"-I=" + g.SourceBase + "/" + g.SourceSubdir,
		"--go_out=" + outDir,
	}
	args = append(args, g.GoOpts...)
	args = append(args, g.ProtoFile)

	print("# Building: " + g.TargetFile)

	// 执行protoc命令
	err = execute("protoc", args)
	if err != nil {
		return err
	}

	// 确定生成的文件路径
	genDir := outDir + "Protobufs/" + g.SourceSubdir + "/" + g.ProtoFile
	genFile := filepath.Join(genDir, strings.Replace(g.ProtoFile, ".proto", ".pb.go", 1))

	// 移动并修复生成的文件
	err = forceRename(genFile, g.TargetFile)
	if err != nil {
		return err
	}

	// 修复生成的protobuf文件
	err = fixProto(g.OutputDir, g.TargetFile)
	if err != nil {
		return err
	}

	// 清理临时文件
	return os.RemoveAll(outDir + "/Protobufs")
}

func buildProto() {
	print("# Building Protobufs")

	buildProtoMap("steam", clientProtoFiles, "../protocol/protobuf/steam")
	buildProtoMap("steam", clientUnifiedExtraProtoFiles, "../protocol/protobuf/steam")
	buildProtoMap("tf2", tf2ProtoFiles, "../protocol/protobuf/tf2")
	buildProtoMap("dota2", dotaProtoFiles, "../protocol/protobuf/dota")
	buildProtoMap("csgo", csgoProtoFiles, "../protocol/protobuf/csgo")

	_ = os.Remove("Protobufs/google/protobuf/valve_extensions.proto")
}

func buildProtoMap(srcSubdir string, files map[string]string, outDir string) {
	_ = os.MkdirAll(outDir, os.ModePerm)

	// 构建通用的Go选项
	var baseOpts []string
	baseOpts = append(baseOpts, "--go_opt=Mgoogle/protobuf/descriptor.proto=google/protobuf/descriptor.proto")
	baseOpts = append(baseOpts, "--go_opt=Mgoogle/protobuf/valve_extensions.proto=google/protobuf/valve_extensions.proto")
	baseOpts = append(baseOpts, "--go_opt=Msteammessages_unified_base.steamworkssdk.proto=steammessages_unified_base.steamworkssdk.proto")
	baseOpts = append(baseOpts, "--go_opt=Msteammessages_steamlearn.steamworkssdk.proto=steammessages_steamlearn.steamworkssdk.proto")

	// 为每个proto文件添加映射选项
	for proto := range files {
		baseOpts = append(baseOpts, "--go_opt=Msteammessages.proto=Protobufs/"+srcSubdir+"/steammessages.proto")
		baseOpts = append(baseOpts, "--go_opt=M"+proto+"=Protobufs/"+srcSubdir+"/"+proto)
	}

	// 添加DOTA2特定选项
	if srcSubdir == "dota2" {
		baseOpts = append(baseOpts, "--go_opt=Mecon_shared_enums.proto=Protobufs/"+srcSubdir+"/econ_shared_enums.proto")
	}

	// 为每个proto文件生成对应的Go文件
	for proto, out := range files {
		targetFile := filepath.Join(outDir, out)

		generator := &ProtobufGenerator{
			BaseGenerator: BaseGenerator{
				OutputDir: outDir,
			},
			SourceBase:   "Protobufs",
			SourceSubdir: srcSubdir,
			ProtoFile:    proto,
			TargetFile:   targetFile,
			GoOpts:       baseOpts,
		}

		err := generator.Run()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error building %s: %v\n", targetFile, err)
			os.Exit(1)
		}
	}
}

func forceRename(from, to string) error {
	if from != to {
		_ = os.Remove(to)
	}
	return os.Rename(from, to)
}

var pkgRegex = regexp.MustCompile(`(package \w+)`)
var unusedImportCommentRegex = regexp.MustCompile("// discarding unused import .*\n")
var fileDescriptorVarRegex = regexp.MustCompile(`fileDescriptor\d+`)

func fixProto(outDir, path string) error {
	// goprotobuf is terrible at dependencies, so we must fix them manually...
	// It tries to load each dependency of a file as a separate package (but in a very, very wrong way).
	// Because we want some files in the same package, we'll remove those imports to local files.

	file, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, file, parser.ImportsOnly)
	if err != nil {
		return fmt.Errorf("Error parsing %s: %v", path, err)
	}

	importsToRemove := make([]*ast.ImportSpec, 0)
	for _, i := range f.Imports {
		if strings.Contains(i.Path.Value, "google/protobuf/descriptor.proto") {
			continue
		}
		// We remove all local imports
		if strings.Contains(i.Path.Value, ".proto") {
			importsToRemove = append(importsToRemove, i)
		}
	}

	for _, itr := range importsToRemove {
		// remove the package name from all types
		file = bytes.Replace(file, []byte(itr.Name.Name+"."), []byte{}, -1)
		// and remove the import itself
		file = bytes.Replace(file, []byte(fmt.Sprintf("%v %v", itr.Name.Name, itr.Path.Value)), []byte{}, -1)
	}

	// remove warnings
	file = unusedImportCommentRegex.ReplaceAllLiteral(file, []byte{})

	// fix the package name
	file = pkgRegex.ReplaceAll(file, []byte("package "+inferPackageName(path)))

	// fix the Google dependency;
	// we just reuse the one from protoc-gen-go
	file = bytes.Replace(file, []byte("google/protobuf/descriptor.proto"), []byte("google.golang.org/protobuf/types/descriptorpb"), -1)

	// we need to prefix local variables created by protoc-gen-go so that they don't clash with others in the same package
	filename := strings.Split(filepath.Base(path), ".")[0]
	file = fileDescriptorVarRegex.ReplaceAllFunc(file, func(match []byte) []byte {
		return []byte(filename + "_" + string(match))
	})

	return os.WriteFile(path, file, os.ModePerm)
}

func inferPackageName(path string) string {
	pieces := strings.Split(path, string(filepath.Separator))
	return pieces[len(pieces)-2]
}

// This writer appends a "> " after every newline so that the output appears quoted.
type quotedWriter struct {
	w       io.Writer
	started bool
}

func newQuotedWriter(w io.Writer) *quotedWriter {
	return &quotedWriter{w, false}
}

func (w *quotedWriter) Write(p []byte) (n int, err error) {
	if !w.started {
		_, err = w.w.Write([]byte("> "))
		if err != nil {
			return n, err
		}
		w.started = true
	}

	for i, c := range p {
		if c == '\n' {
			nw, err := w.w.Write(p[n : i+1])
			n += nw
			if err != nil {
				return n, err
			}

			_, err = w.w.Write([]byte("> "))
			if err != nil {
				return n, err
			}
		}
	}
	if n != len(p) {
		nw, err := w.w.Write(p[n:len(p)])
		n += nw
		return n, err
	}
	return
}

func execute(command string, args []string) error {
	if printCommands {
		print(command + " " + strings.Join(args, " "))
	}

	cmd := exec.Command(command, args...)
	cmd.Stdout = newQuotedWriter(os.Stdout)
	cmd.Stderr = newQuotedWriter(os.Stderr)

	return cmd.Run()
}

// Maps the proto files to their target files.
// See `SteamKit/Resources/Protobufs/steamclient/generate-base.bat` for reference.
var clientProtoFiles = map[string]string{
	"steammessages_base.proto":   "base.pb.go",
	"encrypted_app_ticket.proto": "app_ticket.pb.go",
	"offline_ticket.proto":       "offline_ticket.pb.go",

	"steammessages_clientserver.proto":         "client_server.pb.go",
	"steammessages_clientserver_2.proto":       "client_server_2.pb.go",
	"steammessages_clientserver_friends.proto": "client_server_friends.pb.go",
	"steammessages_clientserver_login.proto":   "client_server_login.pb.go",
	"steammessages_sitelicenseclient.proto":    "client_site_license.pb.go",

	"content_manifest.proto": "content_manifest.pb.go",

	"enums.proto":             "unified/enums.pb.go",
	"enums_productinfo.proto": "unified/enums_productinfo.pb.go",
	"steammessages_unified_base.steamclient.proto":      "unified/base.pb.go",
	"steammessages_auth.steamclient.proto":              "unified/auth.pb.go",
	"steammessages_client_objects.proto":                "unified/client_objects.pb.go",
	"steammessages_cloud.steamclient.proto":             "unified/cloud.pb.go",
	"steammessages_credentials.steamclient.proto":       "unified/credentials.pb.go",
	"steammessages_deviceauth.steamclient.proto":        "unified/deviceauth.pb.go",
	"steammessages_gamenotifications.steamclient.proto": "unified/gamenotifications.pb.go",
	"steammessages_offline.steamclient.proto":           "unified/offline.pb.go",
	"steammessages_parental.steamclient.proto":          "unified/parental.pb.go",
	"steammessages_parental_objects.proto":              "unified/parental_objects.pb.go",
	"steammessages_partnerapps.steamclient.proto":       "unified/partnerapps.pb.go",
	"steammessages_player.steamclient.proto":            "unified/player.pb.go",
	"steammessages_publishedfile.steamclient.proto":     "unified/publishedfile.pb.go",
}

// Duplicate protos that also need to be generated in unified
var clientUnifiedExtraProtoFiles = map[string]string{
	"steammessages_base.proto": "unified/mbase.pb.go",
	"offline_ticket.proto":     "unified/offline_ticket.pb.go",
}

var tf2ProtoFiles = map[string]string{
	"base_gcmessages.proto":  "base.pb.go",
	"econ_gcmessages.proto":  "econ.pb.go",
	"gcsdk_gcmessages.proto": "gcsdk.pb.go",
	"tf_gcmessages.proto":    "tf.pb.go",
	"gcsystemmsgs.proto":     "system.pb.go",
}

var dotaProtoFiles = map[string]string{
	"base_gcmessages.proto":   "base.pb.go",
	"econ_shared_enums.proto": "econ_shared_enum.pb.go",
	"econ_gcmessages.proto":   "econ.pb.go",
	"gcsdk_gcmessages.proto":  "gcsdk.pb.go",
	"gcsystemmsgs.proto":      "system.pb.go",
	"steammessages.proto":     "steam.pb.go",
	"valveextensions.proto":   "valveextensions.pb.go",
}

var csgoProtoFiles = map[string]string{
	"base_gcmessages.proto":        "base.pb.go",
	"cstrike15_gcmessages.proto":   "cstrike15gc.pb.go",
	"cstrike15_usermessages.proto": "cstrike15user.pb.go",
	"econ_gcmessages.proto":        "econ.pb.go",
	"engine_gcmessages.proto":      "enginegc.pb.go",
	"fatdemo.proto":                "fatdemo.pb.go",
	"gcsdk_gcmessages.proto":       "gcsdk.pb.go",
	"gcsystemmsgs.proto":           "system.pb.go",
	"netmessages.proto":            "net.pb.go",
	"network_connection.proto":     "networkconnection.pb.go",
	"steammessages.proto":          "steam.pb.go",
	"uifontfile_format.proto":      "uifontfile.pb.go",
	"networkbasetypes.proto":       "networkbasetypes.pb.go",
}
