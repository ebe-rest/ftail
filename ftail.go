// ftail
//
// Description:
// This is a tool for tailing multiple files similar to `tail -F`. It monitors
// multiple files concurrently and prints new lines appended to them to standard output.
//
// Features:
// - Supports glob patterns (e.g., `/var/log/**/*.log`) to specify multiple files.
// - Resolves symbolic links to avoid watching the same file twice.
// - Files added to the watch list initially are read from the end.
// - New files created that match the glob patterns are automatically added to the watch list.
// - Files that are deleted or renamed are automatically removed from the watch list.
// - The tool efficiently manages file descriptor resources by opening and closing files for each read operation.
//
// Build Instructions:
// A Go compiler is required to build this program. Run the following commands to
// fetch dependencies and create the executable file:
// go mod tidy
// go build -ldflags="-s -w" -o ftail .
//
// Usage:
// Execute the compiled binary or use the `go run` command with one or more
// glob patterns as arguments.
// Example:
// ./ftail --poll-interval 250ms --scan-interval 5s "/var/log/nginx/*.log" "/var/log/apache2/*.access.log"
//
// How It Works:
//  1. **Initialization**: On startup, it finds all existing files that match the
//     specified glob patterns and adds them to a watch list. It resolves symbolic links
//     to prevent duplicate watches.
//  2. **Directory Monitoring**: It uses the `fsnotify` package to watch for `CREATE`,
//     `RENAME`, and `REMOVE` events in directories containing the glob patterns. This allows
//     the tool to react quickly when new files are added.
//  3. **Periodic Scanning**: A regular scan checks for new files that match the glob
//     patterns. This acts as a fallback in case an `fsnotify` event is missed or a file
//     is moved into a watched directory from an unwatched location. The scan interval
//     is configurable via a command-line flag.
//  4. **File Content Polling**: The tool periodically polls each watched file for new
//     content. This polling interval is also configurable. If a file is truncated,
//     it detects this and resets the read position to the beginning.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/fsnotify/fsnotify"
)

// args holds the command-line arguments.
type args struct {
	pollInterval time.Duration
	scanInterval time.Duration
	dispInterval time.Duration
}

// theArgs is a global variable to hold parsed command-line flags.
var theArgs args

// app holds the main state of the ftail application.
type app struct {
	// watchedFiles is a map of files being watched.
	// The key is the file's real path and the value is the read offset.
	watchedFiles sync.Map
	// The key is the dir's real path and the value is the result of error of dirWatcher.Add.
	watchedDirs sync.Map
	// globPatterns is a list of glob patterns provided via command line.
	globPatterns []string
	// dirWatcher is a watcher for directory changes.
	dirWatcher *fsnotify.Watcher
	// args is an anonymous field that allows direct access to the command-line arguments.
	*args
}

// init is executed before the main function to parse flags.
func init() {
	// Parse flags and set the values directly on the _args struct.
	flag.DurationVar(&theArgs.pollInterval, "poll-interval", 500*time.Millisecond, "Interval to poll files for new content")
	flag.DurationVar(&theArgs.scanInterval, "scan-interval", 3*time.Second, "Interval to scan for new files matching glob patterns")
	flag.DurationVar(&theArgs.dispInterval, "disp-interval", 1*time.Minute, "Interval for showing no files changed")
}

// main is the entry point of the application.
func main() {
	flag.Parse()

	// Initialize the application state with a reference to the global args struct.
	a := &app{
		globPatterns: flag.Args(),
		args:         &theArgs, // Embed the global args struct by reference
	}

	if len(a.globPatterns) < 1 {
		_, _ = fmt.Fprintf(os.Stderr, "Usage: %s [flags] <glob_pattern1> [glob_pattern2...]\n", os.Args[0])
		flag.PrintDefaults()
		os.Exit(1)
	}

	// Create a new filesystem watcher for directory events (create, rename, delete).
	var err error
	a.dirWatcher, err = fsnotify.NewWatcher()
	if err != nil {
		log.Printf("Error: creating directory watcher: %v\n", err)
		return
	}
	// Ensure the watcher is closed when the main function exits.
	defer func() { _ = a.dirWatcher.Close() }()

	// Set up the initial set of files to watch based on glob patterns.
	a.setupWatchers()

	// Start a goroutine to handle filesystem events from the directory watcher.
	go a.handleDirEvents()

	// Start a goroutine to poll for file content changes and print to stdout.
	go a.pollFiles()

	// Start a goroutine to periodically scan for new files matching glob patterns.
	go a.scanForNewFiles()

	// Block the main goroutine indefinitely to keep the program running.
	// It will only exit when a signal (e.g., Ctrl+C) is received.
	select {}
}

// setupWatchers initializes the list of files to be watched and sets their initial read offsets.
// It also adds the root directories of the glob patterns to the directory watcher.
func (a *app) setupWatchers() {
	newlyAddedFiles := make(map[string]bool)
	newlyAddedDirs := make(map[string]bool)

	_ = a.globWalk(func(realPath string) error {
		// Add the parent directory to the directory watcher.
		realDir := filepath.Dir(realPath)
		if added := a.addToWatchDir(realDir); added {
			newlyAddedDirs[realDir] = true
		}

		// Add the file to the watch list.
		if added := a.addToWatchFile(realPath); added {
			newlyAddedFiles[realPath] = true
		}

		return nil
	})

	// Remove files that no longer match the glob pattern.
	a.watchedFiles.Range(func(key, _ interface{}) bool {
		path := key.(string)
		if _, ok := newlyAddedFiles[path]; !ok {
			a.handleFileRemoval(path)
		}
		return true
	})

	// Remove directories that no longer contain watched files.
	a.watchedDirs.Range(func(key, _ interface{}) bool {
		dir := key.(string)
		if _, ok := newlyAddedDirs[dir]; !ok {
			if _, loaded := a.watchedDirs.Load(dir); loaded {
				a.handleDirRemoval(dir)
			}
		}
		return true
	})
}

// addToWatchDir adds a directory to the dirWatcher. It returns true if the directory
// was successfully added or was already being watched.
func (a *app) addToWatchDir(realDir string) (added bool) {
	prevErr, loaded := a.watchedDirs.Load(realDir)
	if loaded && prevErr == nil {
		return true
	}

	err := a.dirWatcher.Add(realDir)
	a.watchedDirs.Store(realDir, err)
	if err != nil {
		// A previous attempt to watch this directory failed.
		// Try again, but don't print an error message this time.
		if prevErr != nil {
			return false
		}

		log.Printf("Error: adding directory %s to watcher: %v\n", realDir, err)
		return false
	}

	// This is the first attempt to watch this directory.
	log.Printf("Info: Watching directory: %s\n", realDir)
	return true
}

// addToWatchFile adds a file to the watch list and sets its initial offset.
// It returns true if the file was added, false if it already exists or an error occurred.
func (a *app) addToWatchFile(realPath string) (added bool) {
	if _, ok := a.watchedFiles.Load(realPath); ok {
		return true
	}

	fileInfo, err := os.Stat(realPath)
	if err != nil {
		log.Printf("Error: getting file info for %s: %v\n", realPath, err)
		return false
	}

	offset := fileInfo.Size()
	a.watchedFiles.Store(realPath, offset)
	log.Printf("Info: Watching new file: %s\n", realPath)
	return true
}

// handleFileRemoval removes a file from the watchedFiles map.
func (a *app) handleFileRemoval(path string) {
	a.watchedFiles.Delete(path)
	log.Printf("Info: Stopped watching file: %s\n", path)
}

// handleDirRemoval removes a directory from the dirWatcher.
func (a *app) handleDirRemoval(dir string) {
	if err := a.dirWatcher.Remove(dir); err != nil {
		log.Printf("Error: removing directory %s from watcher: %v\n", dir, err)
	}
	a.watchedDirs.Delete(dir)
	log.Printf("Info: Stopped watching directory: %s\n", dir)
}

// handleDirEvents processes events from the directory watcher.
func (a *app) handleDirEvents() {
	var event fsnotify.Event
	var ok bool
	var err error
	for {
		select {
		case event, ok = <-a.dirWatcher.Events:
			// If the channel is closed, exit the goroutine.
			if !ok {
				return
			}

			// Handle new files created in a watched directory.
			if event.Op&fsnotify.Create != 0 && a.globMatch(event.Name) {
				a.addToWatchFile(event.Name)
			}

			// Handle files removed or renamed from a watched directory.
			if event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				a.handleFileRemoval(event.Name)
			}

		case err, ok = <-a.dirWatcher.Errors:
			// If the error channel is closed, exit the goroutine.
			if !ok {
				return
			}

			log.Printf("Error: Directory watcher error: %v\n", err)
		}
	}
}

// pollFiles periodically polls watched files for new content.
func (a *app) pollFiles() {
	// Create a new Ticker that fires at the specified pollInterval.
	ticker := time.NewTicker(a.pollInterval)
	// Stop the Ticker when this goroutine exits.
	defer ticker.Stop()

	lastContentUpdate := time.Now()

	// The loop waits for the Ticker to fire, ensuring a consistent interval.
	prevPath := ""
	for range ticker.C {
		// Iterate through all currently watched files.
		a.watchedFiles.Range(func(key, value interface{}) bool {
			path := key.(string)
			offset := value.(int64)
			var err error

			// Check if the file still exists on the filesystem.
			var fileInfo os.FileInfo
			fileInfo, err = os.Stat(path)
			if os.IsNotExist(err) {
				// If it doesn't exist, remove it from the watch list.
				a.handleFileRemoval(path)
				return true // Continue to the next iteration.
			}
			if err != nil {
				log.Printf("Error: getting file info for %s: %v\n", path, err)
				return true
			}

			// Open the file to read its contents.
			var file *os.File
			file, err = os.Open(path)
			if err != nil {
				log.Printf("Error: opening file %s: %v\n", path, err)
				return true
			}
			// Ensure the file is closed after returning from this function.
			defer func() { _ = file.Close() }()

			// Check if the file was truncated (current size is smaller than offset).
			currentSize := fileInfo.Size()
			if currentSize < offset {
				log.Printf("Info: File %s truncated, re-reading from start.\n", path)
				offset = 0 // Reset the offset to the beginning of the file.
			}

			// Seek to the last read position.
			_, err = file.Seek(offset, io.SeekStart)
			if err != nil {
				log.Printf("Error: seeking file %s: %v\n", path, err)
				return true
			}

			// Read all new data from the current position to the end of the file.
			var newData []byte
			newData, err = io.ReadAll(file)
			if err != nil {
				log.Printf("Error: reading file %s: %v\n", path, err)
				return true
			}

			if len(newData) <= 0 {
				return true
			}

			if prevPath != path {
				_, _ = fmt.Fprintln(os.Stdout)
				_, _ = fmt.Fprintf(os.Stdout, "--- %s ---\n", path)
				prevPath = path
			}

			_, _ = fmt.Fprint(os.Stdout, string(newData))
			offset += int64(len(newData))
			a.watchedFiles.Store(path, offset) // Store the new offset.

			lastContentUpdate = time.Now() // Update the timestamp when new content is found.
			return true
		})

		// If no new content was read during this poll cycle and the time since the last
		// content update is longer than dispInterval, print a message.
		if a.dispInterval > 0 && time.Since(lastContentUpdate) > a.dispInterval {
			log.Print("Info: no files changed")
			lastContentUpdate = time.Now()
		}
	}
}

// scanForNewFiles periodically scans for new files matching the glob patterns.
func (a *app) scanForNewFiles() {
	// Create a new Ticker that fires at the specified scanInterval.
	ticker := time.NewTicker(a.scanInterval)
	// Stop the Ticker when this goroutine exits.
	defer ticker.Stop()

	// The loop waits for the Ticker to fire, ensuring a consistent interval.
	for range ticker.C {
		a.setupWatchers()
	}
}

func (a *app) globWalk(action func(realPath string) error) error {
	files := make(map[string]bool)
	for _, p := range a.globPatterns {
		// Split the glob pattern into the base directory and the rest of the pattern.
		base, pattern := doublestar.SplitPattern(p)
		fs := os.DirFS(base)
		// Use doublestar.GlobWalk to match bash-like globs with a callback.
		err := doublestar.GlobWalk(fs, pattern, func(path string, d os.DirEntry) (err error) {
			resolvedPath := filepath.Join(base, path)

			absolutePath, err := filepath.Abs(resolvedPath)
			if err != nil {
				absolutePath = resolvedPath
			}

			// Resolve symlinks and get the real path.
			realPath, err := filepath.EvalSymlinks(absolutePath)
			if err != nil {
				realPath = absolutePath
			}

			if files[realPath] {
				return nil
			}

			err = action(realPath)
			if err != nil {
				return err
			}

			files[realPath] = true
			return nil
		})
		if err != nil {
			log.Printf("Error: with glob pattern %s: %v\n", p, err)
		}
	}
	return nil
}

func (a *app) globMatch(realPath string) bool {
	found := errors.New("glob is match")
	err := a.globWalk(func(path string) error {
		if path == realPath {
			return found
		}
		return nil
	})
	return errors.Is(err, found)
}
