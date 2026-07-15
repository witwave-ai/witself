package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/witwave-ai/witself/internal/client"
)

const maxMemoryVectorFileBytes = 256 * 1024

func memoryVector(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself memory vector profile create|list ... | set ...")
		return 2
	}
	if args[0] == "profile" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: witself memory vector profile create|list ...")
			return 2
		}
		switch args[1] {
		case "create", "set":
			return memoryVectorProfileCreate(args[2:])
		case "list", "ls":
			return memoryVectorProfileList(args[2:])
		}
	}
	if args[0] == "set" || args[0] == "put" {
		return memoryVectorSet(args[1:])
	}
	fmt.Fprintln(os.Stderr, "usage: witself memory vector profile create|list ... | set ...")
	return 2
}

func memoryVectorProfileCreate(args []string) int {
	fs := flag.NewFlagSet("memory vector profile create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	provider := fs.String("provider", "", "client-side vector provider identifier")
	model := fs.String("model", "", "client-side model identifier")
	recipe := fs.String("recipe", "", "client-side input/preprocessing recipe")
	recipeVersion := fs.String("recipe-version", "", "immutable recipe version")
	dimensions := fs.Int("dimensions", 0, "vector dimensions from 1 to 4096")
	metric := fs.String("metric", "cosine", "cosine, dot, or euclidean")
	normalization := fs.String("normalization", "none", "none or l2")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || strings.TrimSpace(*provider) == "" || strings.TrimSpace(*model) == "" ||
		strings.TrimSpace(*recipe) == "" || strings.TrimSpace(*recipeVersion) == "" || *dimensions < 1 {
		fmt.Fprintln(os.Stderr, "usage: witself memory vector profile create --provider P --model M --recipe R --recipe-version V --dimensions N [--metric cosine] [--normalization none]")
		return 2
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	profile, err := client.CreateMemoryVectorProfile(ctx, conn.Endpoint, conn.Token, client.CreateMemoryVectorProfileInput{
		Provider: *provider, Model: *model, Recipe: *recipe, RecipeVersion: *recipeVersion,
		Dimensions: *dimensions, DistanceMetric: *metric, Normalization: *normalization,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: create memory vector profile: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(profile)
	}
	fmt.Printf("%s\t%s\t%s\t%d\t%s\t%s\t%s\n", profile.ID, profile.Provider,
		profile.Model, profile.Dimensions, profile.DistanceMetric, profile.Normalization, profile.ContractHash)
	return 0
}

func memoryVectorProfileList(args []string) int {
	fs := flag.NewFlagSet("memory vector profile list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		return 2
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	profiles, err := client.ListMemoryVectorProfiles(ctx, conn.Endpoint, conn.Token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: list memory vector profiles: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(map[string]any{"items": profiles})
	}
	w, flush := tableWriter("id\tprovider\tmodel\tdimensions\tmetric\tnormalization\trecipe\tversion")
	for _, profile := range profiles {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\n", profile.ID,
			tabSafe(profile.Provider), tabSafe(profile.Model), profile.Dimensions,
			profile.DistanceMetric, profile.Normalization, tabSafe(profile.Recipe),
			tabSafe(profile.RecipeVersion))
	}
	flush()
	return 0
}

func memoryVectorSet(args []string) int {
	memoryID, args := memoryLeadingID(args)
	fs := flag.NewFlagSet("memory vector set", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	account, realm, agent, endpoint, tokenFile := factConnectionFlags(fs)
	profileID := fs.String("profile", "", "immutable vector profile id")
	version := fs.Int64("memory-version", 0, "exact immutable memory version")
	contentHash := fs.String("content-hash", "", "exact memory version content hash")
	vectorFile := fs.String("vector-file", "", "JSON array of finite vector numbers ('-' means stdin)")
	jsonOut := jsonFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if memoryID == "" && fs.NArg() == 1 {
		memoryID = strings.TrimSpace(fs.Arg(0))
	} else if fs.NArg() != 0 {
		return 2
	}
	vector, err := readMemoryVectorFile(*vectorFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 2
	}
	if memoryID == "" || strings.TrimSpace(*profileID) == "" || *version < 1 ||
		strings.TrimSpace(*contentHash) == "" || len(vector) == 0 {
		fmt.Fprintln(os.Stderr, "usage: witself memory vector set MEM_ID --profile ID --memory-version N --content-hash SHA256 --vector-file FILE")
		return 2
	}
	ctx := context.Background()
	conn, err := connectAgent(ctx, *account, *realm, *agent, *endpoint, *tokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: %v\n", err)
		return 1
	}
	receipt, err := client.PutMemoryVector(ctx, conn.Endpoint, conn.Token, client.PutMemoryVectorInput{
		ProfileID: *profileID, MemoryID: memoryID, MemoryVersion: *version,
		ContentHash: *contentHash, Vector: vector,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "witself: put memory vector: %v\n", err)
		return 1
	}
	if *jsonOut {
		return printJSON(receipt)
	}
	fmt.Printf("stored\tprofile=%s\tmemory=%s@%d\tdimensions=%d\tvector-hash=%s\treplayed=%t\n",
		receipt.ProfileID, receipt.MemoryID, receipt.MemoryVersion, receipt.Dimensions,
		receipt.VectorHash, receipt.Replayed)
	return 0
}

func readMemoryVectorFile(path string) ([]float64, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	var (
		reader io.Reader
		file   *os.File
	)
	if path == "-" {
		reader = os.Stdin
	} else {
		var err error
		file, err = os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("read vector file: %w", err)
		}
		defer func() { _ = file.Close() }()
		reader = file
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maxMemoryVectorFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read vector file: %w", err)
	}
	if len(raw) > maxMemoryVectorFileBytes {
		return nil, fmt.Errorf("read vector file: input exceeds %d bytes", maxMemoryVectorFileBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var numbers []json.Number
	if err := decoder.Decode(&numbers); err != nil {
		return nil, fmt.Errorf("decode vector file: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("decode vector file: expected one JSON array")
	}
	vector := make([]float64, len(numbers))
	for i, number := range numbers {
		value, err := number.Float64()
		if err != nil {
			return nil, fmt.Errorf("decode vector file component %d: %w", i, err)
		}
		vector[i] = value
	}
	return vector, nil
}
