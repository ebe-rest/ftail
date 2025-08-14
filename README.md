# **ftail: A Concurrent Log Tailing Tool**

ftail is a command-line utility for concurrently monitoring and tailing multiple log files. It is designed to be a more flexible alternative to tail \-F, especially for environments where log files are rotated, created, or deleted frequently. The tool supports glob patterns, automatically detects new files, and handles symbolic links.

### **Features**

* **Glob Pattern Support:** Watch multiple files using familiar glob patterns (e.g., /var/log/\*\*/\*.log).
* **Symbolic Link Resolution:** Accurately tracks files even when they are accessed via symbolic links, preventing duplicate watches.
* **Real-time File Watching:** Uses fsnotify to instantly detect file creation, deletion, or renaming within watched directories.
* **Periodic Scanning:** A fallback mechanism that periodically scans for new files, ensuring no files are missed even if filesystem events are not captured.
* **Resource Efficiency:** Manages file descriptors by opening and closing files for each poll, which is suitable for environments with a large number of inactive files.

### **Build Instructions**

To build the ftail executable, you need a Go compiler installed. The following commands will fetch the necessary dependencies and create a lightweight, optimized binary.

```
# Fetch the required dependencies  
# Fetches and cleans up module dependencies.  
go mod tidy

# Compiles the program.
# The -ldflags="-s -w" part removes the symbol table and debugging information, significantly reducing the size of the final binary.
go build \-ldflags="-s \-w" \-o ftail .
```

### **Usage**

Execute the compiled binary with one or more glob patterns as arguments. You can also customize the polling and scanning intervals using flags.

```
# Example usage with glob patterns  
./ftail "/var/log/nginx/*.log" "/var/log/apache2/*.access.log"

# Example with custom intervals  
./ftail --poll-interval 250ms --scan-interval 5s "/var/log/**/*.log"
```

#### **Command-line Flags**

| Flag             | Default | Description                                                                                             |
|:-----------------|:--------|:--------------------------------------------------------------------------------------------------------|
| \--poll-interval | 500ms   | The interval to poll watched files for new content.                                                     |
| \--scan-interval | 3s      | The interval to scan for new files matching glob patterns.                                              |
| \--disp-interval | 1m      | The interval to display "no files changed" if nothing has happened. A value of 0 disables this message. |

### **Implementation Details**

ftail is built with a highly concurrent architecture to handle multiple file watches efficiently.

* **Main Application State:** The core logic is managed by an app struct, which holds the list of watched files and directories, the fsnotify watcher, and command-line arguments.
* **Concurrency:** The application launches three main goroutines:
    1. handleDirEvents(): A dedicated goroutine that listens for fsnotify events (create, remove, rename) to react to filesystem changes in real time.
    2. pollFiles(): A goroutine with a ticker that periodically polls each watched file for new data, prints it, and updates the read offset.
    3. scanForNewFiles(): A periodic goroutine that re-runs the initial file search to catch any changes that fsnotify may have missed.
* **Thread-Safe Data:** A sync.Map is used for watchedFiles to ensure safe, concurrent access from multiple goroutines without explicit locking.
* **Glob Pattern Handling:** The doublestar library is used to handle flexible glob patterns, including recursive wildcards (\*\*).
* **Error Handling:** All error and info messages are directed to standard error (os.Stderr) using log.Printf to keep them separate from the application's primary output (the file content itself, which is sent to os.Stdout).
