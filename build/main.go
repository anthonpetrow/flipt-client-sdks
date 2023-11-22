package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"dagger.io/dagger"
	"golang.org/x/sync/errgroup"
)

var (
	languages    string
	languageToFn = map[string]integrationTestFn{
		"python": pythonTests,
		"go":     goTests,
	}
	sema = make(chan struct{}, 2)
)

type integrationTestFn func(context.Context, *dagger.Client, *dagger.Container, *dagger.File, *dagger.File, *dagger.Directory) error

func init() {
	flag.StringVar(&languages, "languages", "", "comma separated list of which language(s) to run integration tests for")
}

func main() {
	flag.Parse()

	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	var languagesTestsMap = map[string]integrationTestFn{
		"python": pythonTests,
		"go":     goTests,
	}

	if languages != "" {
		l := strings.Split(languages, ",")
		tlm := make(map[string]integrationTestFn, len(l))
		for _, language := range l {
			if testFn, ok := languageToFn[language]; !ok {
				return fmt.Errorf("language %s is not supported", language)
			} else {
				tlm[language] = testFn
			}
		}

		languagesTestsMap = tlm
	}

	ctx := context.Background()

	client, err := dagger.Connect(ctx, dagger.WithLogOutput(os.Stderr))
	if err != nil {
		return err
	}
	defer client.Close()

	dir := client.Host().Directory(".")

	flipt, dynamicLibrary, headerFile := getTestDependencies(ctx, client, dir)

	var g errgroup.Group

	for _, testFn := range languagesTestsMap {
		err := testFn(ctx, client, flipt, dynamicLibrary, headerFile, dir)
		if err != nil {
			return err
		}
		g.Go(take(func() error {
			err := testFn(ctx, client, flipt, dynamicLibrary, headerFile, dir)
			return err
		}))
	}

	err = g.Wait()

	return err
}

func take(fn func() error) func() error {
	return func() error {
		// insert into semaphore channel to maintain
		// a max concurrency
		sema <- struct{}{}
		defer func() { <-sema }()

		return fn()
	}
}

// getTestDependencies builds the dynamic library for the Rust core, and the Flipt container for the client libraries to run
// their tests against.
func getTestDependencies(ctx context.Context, client *dagger.Client, hostDirectory *dagger.Directory) (*dagger.Container, *dagger.File, *dagger.File) {
	// Dynamic Library
	rust := client.Container().From("rust:1.73.0-bookworm").
		WithWorkdir("/app").
		WithDirectory("/app/flipt-engine", hostDirectory.Directory("flipt-engine")).
		WithFile("/app/Cargo.toml", hostDirectory.File("Cargo.toml")).
		WithExec([]string{"cargo", "build", "--release"})

	// Flipt
	flipt := client.Container().From("flipt/flipt:nightly").
		WithUser("root").
		WithExec([]string{"mkdir", "-p", "/var/data/flipt"}).
		WithDirectory("/var/data/flipt", hostDirectory.Directory("build/fixtures/testdata")).
		WithExec([]string{"chown", "-R", "flipt:flipt", "/var/data/flipt"}).
		WithUser("flipt").
		WithEnvVariable("FLIPT_STORAGE_TYPE", "local").
		WithEnvVariable("FLIPT_STORAGE_LOCAL_PATH", "/var/data/flipt").
		WithExposedPort(8080)

	return flipt, rust.File("/app/target/release/libfliptengine.so"), rust.File("/app/target/release/flipt_engine.h")
}

// pythonTests runs the Poetry test suite against a container running Flipt through the dynamic library.
func pythonTests(ctx context.Context, client *dagger.Client, flipt *dagger.Container, dynamicLibrary *dagger.File, _ *dagger.File, hostDirectory *dagger.Directory) error {
	_, err := client.Container().From("python:3.11-bookworm").
		WithExec([]string{"pip", "install", "poetry==1.7.0"}).
		WithWorkdir("/app").
		WithDirectory("/app", hostDirectory.Directory("flipt-client-python")).
		WithFile("/app/libfliptengine.so", dynamicLibrary).
		WithServiceBinding("flipt", flipt.WithExec(nil).AsService()).
		WithEnvVariable("FLIPT_URL", "http://flipt:8080").
		WithEnvVariable("FLIPT_ENGINE_LIB_PATH", "/app/libfliptengine.so").
		WithExec([]string{"poetry", "install", "--without=dev"}).
		WithExec([]string{"poetry", "run", "test"}).
		Sync(ctx)

	return err
}

// goTests runs the golang integration tests suite embedding a C header file and the Rust dynamic library.
func goTests(ctx context.Context, client *dagger.Client, flipt *dagger.Container, dynamicLibrary *dagger.File, headerFile *dagger.File, hostDirectory *dagger.Directory) error {
	_, err := client.Container().From("golang:1.21.3-bookworm").
		WithExec([]string{"apt-get", "update"}).
		WithExec([]string{"apt-get", "-y", "install", "build-essential"}).
		WithWorkdir("/app").
		WithDirectory("/app", hostDirectory.Directory("flipt-client-go")).
		WithFile("/app/libfliptengine.so", dynamicLibrary).
		WithFile("/app/flipt_engine.h", headerFile).
		WithServiceBinding("flipt", flipt.WithExec(nil).AsService()).
		WithEnvVariable("FLIPT_URL", "http://flipt:8080").
		// Since the dynamic library is being sourced from a "non-standard location" we can
		// modify the LD_LIBRARY_PATH variable to inform the linker different locations for
		// dynamic libraries.
		WithEnvVariable("LD_LIBRARY_PATH", "/app:$LD_LIBRARY_PATH").
		WithExec([]string{"go", "mod", "download"}).
		WithExec([]string{"go", "test", "./..."}).
		Sync(ctx)

	return err
}