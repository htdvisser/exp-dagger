package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path"
	"time"

	"dagger.io/dagger"
	platformFormat "github.com/containerd/containerd/platforms"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := build(ctx); err != nil {
		fmt.Println(err)
	}
}

func build(ctx context.Context) error {
	log.Println("Building with Dagger")

	client, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stdout))
	if err != nil {
		return err
	}
	defer client.Close()

	binaries, err := buildBinaries(ctx, client)
	if err != nil {
		return err
	}

	_, err = binaries.Export(ctx, "./build")
	if err != nil {
		return err
	}

	images, err := buildImages(ctx, client, binaries)
	if err != nil {
		return err
	}

	err = pushImages(ctx, client, images)
	if err != nil {
		return err
	}

	return nil
}

var (
	buildImageName = "golang:1.20"
	buildBinary    = "main"
	buildGOOSs     = []string{"linux", "darwin", "windows"}
	buildGOARCHs   = []string{"amd64", "arm64"}
)

func buildBinaries(ctx context.Context, client *dagger.Client) (*dagger.Directory, error) {
	src := client.Host().Directory(".")
	dst := client.Directory()

	goModCache := client.CacheVolume("go-mod")

	buildContainer := client.Container().
		From(buildImageName).
		WithDirectory("/src", src).
		WithWorkdir("/src").
		WithMountedCache("/go/mod", goModCache).
		WithEnvVariable("GOMODCACHE", "/go/mod").
		WithEnvVariable("CGO_ENABLED", "0")

	for _, goos := range buildGOOSs {
		for _, goarch := range buildGOARCHs {
			goBuildCache := client.CacheVolume(fmt.Sprintf("go-build-%s-%s", goos, goarch))

			filename := fmt.Sprintf("%s-%s-%s", buildBinary, goos, goarch)
			out := path.Join("build", filename)
			if goos == "windows" {
				out += ".exe"
			}

			build := buildContainer.
				WithMountedCache("/go/build", goBuildCache).
				WithEnvVariable("GOCACHE", "/go/build").
				WithEnvVariable("GOOS", goos).
				WithEnvVariable("GOARCH", goarch).
				WithExec([]string{"go", "build", "-o", out})

			dst = dst.WithFile(filename, build.File(out))
		}
	}

	return dst, nil
}

var imagePlatforms = []dagger.Platform{"linux/amd64", "linux/arm64"}

func architectureOf(platform dagger.Platform) string {
	return platformFormat.MustParse(string(platform)).Architecture
}

func buildImages(ctx context.Context, client *dagger.Client, build *dagger.Directory) ([]*dagger.Container, error) {
	platformVariants := make([]*dagger.Container, 0, len(imagePlatforms))
	for _, imagePlatform := range imagePlatforms {
		container := client.Container(dagger.ContainerOpts{Platform: imagePlatform}).
			WithFile("/main", build.File(fmt.Sprintf("main-linux-%s", architectureOf(imagePlatform)))).
			WithEntrypoint([]string{"/main"})
		platformVariants = append(platformVariants, container)
	}
	return platformVariants, nil
}

var (
	ciProjectName      = os.Getenv("CI_PROJECT_NAME")
	ciProjectURL       = os.Getenv("CI_PROJECT_URL")
	ciCommitSHA        = os.Getenv("CI_COMMIT_SHA")
	ciCommitBranch     = os.Getenv("CI_COMMIT_BRANCH")
	ciDefaultBranch    = os.Getenv("CI_DEFAULT_BRANCH")
	ciRegistry         = os.Getenv("CI_REGISTRY")
	ciRegistryUser     = os.Getenv("CI_REGISTRY_USER")
	ciRegistryPassword = os.Getenv("CI_REGISTRY_PASSWORD")
	ciRegistryImage    = os.Getenv("CI_REGISTRY_IMAGE")
)

func pushImages(ctx context.Context, client *dagger.Client, platformVariants []*dagger.Container) error {
	container := client.
		Container().
		WithLabel("org.opencontainers.image.created", time.Now().String())

	if ciProjectName != "" {
		container = container.WithLabel("org.opencontainers.image.title", ciProjectName)
	}
	if ciProjectURL != "" {
		container = container.WithLabel("org.opencontainers.image.source", ciProjectURL)
	}
	if ciCommitSHA != "" {
		container = container.WithLabel("org.opencontainers.image.revision", ciCommitSHA)
	}

	if ciRegistry != "" && ciRegistryUser != "" && ciRegistryPassword != "" {
		ciRegistryPasswordSecret := client.SetSecret("ci_registry_password", ciRegistryPassword)
		container = container.WithRegistryAuth(ciRegistry, ciRegistryUser, ciRegistryPasswordSecret)
	}

	if ciRegistryImage == "" {
		_, err := container.Export(ctx, "./build/image.tar", dagger.ContainerExportOpts{
			PlatformVariants: platformVariants,
		})
		return err
	}

	tags := []string{
		fmt.Sprintf("%s:%s", ciRegistryImage, ciCommitSHA),
	}
	if ciCommitBranch != "" {
		tags = append(tags, fmt.Sprintf("%s:%s", ciRegistryImage, ciCommitBranch))
	}
	if ciCommitBranch == ciDefaultBranch {
		tags = append(tags, fmt.Sprintf("%s:latest", ciRegistryImage))
	}

	for _, tag := range tags {
		digest, err := container.Publish(ctx, ciRegistryImage, dagger.ContainerPublishOpts{
			PlatformVariants: platformVariants,
		})
		if err != nil {
			return err
		}
		log.Printf("Image digest for tag %q: %q", tag, digest)
	}

	return nil
}
