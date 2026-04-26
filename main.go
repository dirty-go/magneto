package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/anacrolix/torrent"
)

var (
	magnetLink = flag.String("magnet", "", "Magnet link")
	outputDir  = flag.String("out", "", "Download directory")
	noSeed     = flag.Bool("no-seed", true, "Disable seeding after download")
)

func main() {
	flag.Parse()

	if *magnetLink == "" {
		log.Fatal("magnet link is required")
	}

	// If output directory is not provided, use "Downloads" in the current directory
	if outputDir == nil || *outputDir == "" {
		_, filename, _, _ := runtime.Caller(0)
		dir := filepath.Dir(filename)
		downloadDir := filepath.Join(dir, "Downloads")
		stats, err := os.Stat(downloadDir)
		if err == nil {
			if stats.IsDir() {
				outputDir = &downloadDir
			}
		} else if errors.Is(err, os.ErrNotExist) {
			fmt.Println("Directory does not exist.")
			err := os.MkdirAll(*&downloadDir, 0755)
			if err != nil {
				log.Fatal(err)
				os.Exit(0)
			}
			outputDir = &downloadDir
		} else {
			// Some other error occurred (e.g., permissions)
			log.Fatalf("Error checking directory: %w\n", err)
			os.Exit(0)
		}
	}

	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = *outputDir
	cfg.NoUpload = *noSeed

	client, err := torrent.NewClient(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	t, err := client.AddMagnet(*magnetLink)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("Fetching metadata...")
	<-t.GotInfo()

	fmt.Println("Torrent name:", t.Name())

	// Download only media files
	mediaExt := map[string]bool{
		".mp4":  true,
		".mkv":  true,
		".avi":  true,
		".mp3":  true,
		".flac": true,
		".wav":  true,
	}

	var totalSelected int64

	for _, f := range t.Files() {
		ext := strings.ToLower(filepath.Ext(f.Path()))
		if mediaExt[ext] {
			fmt.Println("Downloading:", f.Path())
			f.Download()
			totalSelected += f.Length()
		}
	}

	if totalSelected == 0 {
		log.Fatal("No media files found in torrent")
	}

	// Progress ticker
	go func() {
		for range time.Tick(2 * time.Second) {
			stats := t.Stats()
			downloaded := stats.BytesReadData.Int64()
			percent := float64(downloaded) / float64(totalSelected) * 100
			fmt.Printf(
				"\rProgress: %6.2f%% | Downloaded: %d MB | Peers: %d",
				percent,
				downloaded/1024/1024,
				stats.ActivePeers,
			)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)

	select {
	case <-t.Complete().On():
		fmt.Println("\nDownload completed")
	case <-sig:
		fmt.Println("\nInterrupted, saving progress…")
	}

	os.Exit(0)
}
