package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var testsRunning atomic.Bool

var defaultServers = []string{
	"speedtest.ams1.nl.leaseweb.net",
	"ams.speedtest.clouvider.net",
	"scaleway.testdebit.info",
	"speedtest.kamel.network",
}

type SpeedTestResult struct {
	Server    string
	Direction string
	Speed     float64
}

func initDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, err
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS speed_tests (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			server TEXT NOT NULL,
			direction TEXT NOT NULL,
			speed REAL NOT NULL,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return nil, err
	}

	return db, nil
}

func runIperf3Test(ctx context.Context, server, direction string, parallelStreams int) (*SpeedTestResult, error) {
	args := []string{"-c", server, "-J"}
	if parallelStreams > 0 {
		args = append(args, "-P", strconv.Itoa(parallelStreams))
	}
	if direction == "upload" {
		args = append(args, "-R")
	}

	//log.Printf("Executing iperf3 command: iperf3 %s", strings.Join(args, " "))

	// create a new context with timeout
	testCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(testCtx, "iperf3", args...)
	output, err := cmd.CombinedOutput()

	if testCtx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("iperf3 test timed out after 30 seconds")
	}

	if ctx.Err() == context.Canceled {
		return nil, fmt.Errorf("iperf3 test was cancelled")
	}

	if err != nil {
		return nil, fmt.Errorf("iperf3 command failed: %v\nOutput: %s", err, string(output))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(output, &result); err != nil {
		return nil, fmt.Errorf("failed to parse iperf3 JSON output: %v", err)
	}

	end, ok := result["end"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("could not find 'end' data in iperf3 output")
	}

	var speedData map[string]interface{}
	if direction == "download" {
		speedData, ok = end["sum_received"].(map[string]interface{})
	} else {
		speedData, ok = end["sum_sent"].(map[string]interface{})
	}
	if !ok {
		return nil, fmt.Errorf("could not find speed data for %s test", direction)
	}

	bitsPerSecond, ok := speedData["bits_per_second"].(float64)
	if !ok {
		return nil, fmt.Errorf("could not find 'bits_per_second' in iperf3 output")
	}

	speed := bitsPerSecond / 1e6 // convert to Mbps

	log.Printf("Completed %s test for server %s: %.2f Mbps", direction, server, speed)

	return &SpeedTestResult{
		Server:    server,
		Direction: direction,
		Speed:     speed,
	}, nil
}

func runTestWithRetry(ctx context.Context, server, direction string, maxRetries, parallelStreams int) (*SpeedTestResult, error) {
	for i := 0; i < maxRetries; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			log.Printf("Attempting %s test for server %s (attempt %d/%d)", direction, server, i+1, maxRetries)
			result, err := runIperf3Test(ctx, server, direction, parallelStreams)
			if err == nil {
				return result, nil
			}

			log.Printf("Error during test: %v", err)

			if strings.Contains(err.Error(), "busy") {
				log.Printf("Server %s is busy. Retrying in 2 seconds...", server)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(2 * time.Second):
				}
			} else {
				return nil, err
			}
		}
	}
	return nil, fmt.Errorf("server %s is busy after %d attempts", server, maxRetries)
}

func checkIperf3() error {
	cmd := exec.Command("iperf3", "--version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("iperf3 check failed: %v\nOutput: %s", err, string(output))
	}
	//log.Printf("iperf3 version: %s", string(output))
	return nil
}

func storeResult(db *sql.DB, result *SpeedTestResult) error {
	_, err := db.Exec(`
		INSERT INTO speed_tests (server, direction, speed)
		VALUES (?, ?, ?)
	`, result.Server, result.Direction, result.Speed)
	return err
}

func displayResults(db *sql.DB) error {
	rows, err := db.Query(`
		SELECT server, direction, AVG(speed) as avg_speed, MAX(speed) as max_speed, MIN(speed) as min_speed
		FROM speed_tests
		GROUP BY server, direction
		ORDER BY server, direction
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	fmt.Println("Speed Test Results:")
	fmt.Printf("%-30s %-10s %-15s %-15s %-15s\n", "Server", "Direction", "Avg (Mbps)", "Max (Mbps)", "Min (Mbps)")
	fmt.Println(strings.Repeat("-", 90))

	for rows.Next() {
		var server, direction string
		var avgSpeed, maxSpeed, minSpeed float64
		err := rows.Scan(&server, &direction, &avgSpeed, &maxSpeed, &minSpeed)
		if err != nil {
			return err
		}
		fmt.Printf("%-30s %-10s %-15.2f %-15.2f %-15.2f\n", server, direction, avgSpeed, maxSpeed, minSpeed)
	}

	return nil
}

func runAllTests(ctx context.Context, db *sql.DB, servers []string, maxRetries, parallelStreams int) {
	for _, server := range servers {
		if err := checkServerConnectivity(server); err != nil {
			log.Printf("Server %s is not reachable: %v", server, err)
			continue
		}

		// run download test
		downloadResult, err := runTestWithRetry(ctx, server, "download", maxRetries, parallelStreams)
		if err != nil {
			if ctx.Err() != nil {
				log.Println("Test run interrupted")
				return
			}
			log.Printf("Error running download test for %s: %v", server, err)
		} else {
			if err := storeResult(db, downloadResult); err != nil {
				log.Printf("Error storing download result for %s: %v", server, err)
			} //else {
			//log.Printf("Stored download result for %s: %.2f Mbps", server, downloadResult.Speed)
			//}
		}

		// wait before starting the upload test
		if !waitOrShutdown(ctx, 2*time.Second) {
			return
		}

		// run upload test
		uploadResult, err := runTestWithRetry(ctx, server, "upload", maxRetries, parallelStreams)
		if err != nil {
			if ctx.Err() != nil {
				log.Println("Test run interrupted, shutting down...")
				return
			}
			log.Printf("Error running upload test for %s: %v", server, err)
		} else {
			if err := storeResult(db, uploadResult); err != nil {
				log.Printf("Error storing upload result for %s: %v", server, err)
			} //else {
			//log.Printf("Stored upload result for %s: %.2f Mbps", server, uploadResult.Speed)
			//}
		}

		// wait before moving to the next server
		if !waitOrShutdown(ctx, 2*time.Second) {
			return
		}
	}

	if err := displayResults(db); err != nil {
		log.Printf("Error displaying results: %v", err)
	}
}

func waitOrShutdown(ctx context.Context, duration time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(duration):
		return true
	}
}

func checkServerConnectivity(server string) error {
	//log.Printf("Checking connectivity to %s", server)
	cmd := exec.Command("ping", "-c", "3", "-W", "5", server)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to ping %s: %v\nOutput: %s", server, err, string(output))
	}
	//log.Printf("Server %s is reachable", server)
	return nil
}

func main() {
	//log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.SetFlags(log.LstdFlags)

	dbPath := flag.String("db", "speed_tests.db", "Path to the SQLite database file")
	serversFlag := flag.String("servers", "", fmt.Sprintf("Comma-separated list of iperf3 servers. If not specified, defaults to: %s", strings.Join(defaultServers, ", ")))
	maxRetries := flag.Int("retries", 2, "Maximum number of retries for busy servers")
	intervalFlag := flag.Duration("interval", 1*time.Hour, "Interval between test runs (e.g., 30m, 1h, 24h)")
	parallelStreams := flag.Int("P", 8, "Number of parallel streams for iperf3 tests")
	flag.Parse()

	// check if iperf3 is available
	if err := checkIperf3(); err != nil {
		log.Fatalf("iperf3 not available: %v", err)
	}

	var servers []string
	if *serversFlag != "" {
		servers = strings.Split(*serversFlag, ",")
	} else {
		servers = defaultServers
	}

	db, err := initDB(*dbPath)
	if err != nil {
		log.Fatalf("Error initializing database: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	// run tests at specified intervals
	ticker := time.NewTicker(*intervalFlag)
	defer ticker.Stop()

	log.Printf("Starting speed tests. Will run every %v", *intervalFlag)

	// run the first test immediately
	testsRunning.Store(true)
	go func() {
		runAllTests(ctx, db, servers, *maxRetries, *parallelStreams)
		testsRunning.Store(false)
		log.Printf("Next test scheduled at: %s", time.Now().Add(*intervalFlag).Format("2006-01-02 15:04:05"))
	}()

	for {
		select {
		case <-ticker.C:
			if !testsRunning.Load() {
				testsRunning.Store(true)
				go func() {
					runAllTests(ctx, db, servers, *maxRetries, *parallelStreams)
					testsRunning.Store(false)
					log.Printf("Next test scheduled at: %s", time.Now().Add(*intervalFlag).Format("2006-01-02 15:04:05"))
				}()
			} else {
				log.Println("Previous test still running, skipping this interval")
			}
		case <-quit:
			log.Println("Received shutdown signal.")
			cancel()

			if testsRunning.Load() {
				log.Println("Waiting for ongoing tests to complete...")
				for testsRunning.Load() {
					time.Sleep(500 * time.Millisecond)
				}
			}

			log.Println("Shutting down gracefully...")
			return
		}
	}
}
