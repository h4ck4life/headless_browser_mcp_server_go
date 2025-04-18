package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/chromedp/cdproto/dom"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Constants for configuration
const (
	DEFAULT_TIMEOUT     = 60 // seconds - increased from 30 to 60
	DEFAULT_USER_AGENT  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	MAX_CONTENT_LENGTH  = 100000 // characters
	DEFAULT_WAIT_TIME   = 10     // seconds to wait for page to load - increased from 5 to 10
	DEFAULT_SCROLL_TIME = 2      // seconds to scroll the page
)

// Configuration for the browser
type BrowserConfig struct {
	Timeout        int
	UserAgent      string
	MaxContentLen  int
	DefaultWaitSec int
	ScrollTimeSec  int
	ChromePath     string
	Headless       bool
}

func getBrowserConfig() *BrowserConfig {
	return &BrowserConfig{
		Timeout:        getEnvIntOrDefault("BROWSER_TIMEOUT", DEFAULT_TIMEOUT),
		UserAgent:      getEnvOrDefault("BROWSER_USER_AGENT", DEFAULT_USER_AGENT),
		MaxContentLen:  getEnvIntOrDefault("BROWSER_MAX_CONTENT_LENGTH", MAX_CONTENT_LENGTH),
		DefaultWaitSec: getEnvIntOrDefault("BROWSER_WAIT_TIME", DEFAULT_WAIT_TIME),
		ScrollTimeSec:  getEnvIntOrDefault("BROWSER_SCROLL_TIME", DEFAULT_SCROLL_TIME),
		ChromePath:     getEnvOrDefault("CHROME_PATH", ""),
		Headless:       getEnvBoolOrDefault("BROWSER_HEADLESS", true),
	}
}

func getEnvOrDefault(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

func getEnvIntOrDefault(key string, defaultValue int) int {
	if value, exists := os.LookupEnv(key); exists {
		var result int
		_, err := fmt.Sscanf(value, "%d", &result)
		if err == nil {
			return result
		}
	}
	return defaultValue
}

func getEnvBoolOrDefault(key string, defaultValue bool) bool {
	if value, exists := os.LookupEnv(key); exists {
		return strings.ToLower(value) == "true" || value == "1"
	}
	return defaultValue
}

func fetchWebpage(url string, waitSeconds int, extractText bool) (string, error) {
	config := getBrowserConfig()

	// Create a new context with a longer timeout
	baseCtx := context.Background()
	ctx, cancel := context.WithTimeout(baseCtx, time.Duration(config.Timeout)*time.Second)
	defer cancel()

	// Set up browser options
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-web-security", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("ignore-certificate-errors", true),
		chromedp.UserAgent(config.UserAgent),
		chromedp.WindowSize(1920, 1080),
	)

	// Add headless mode option
	if config.Headless {
		// Use the headless flag instead of the Headless function
		opts = append(opts, chromedp.Flag("headless", true))
	}

	// If ChromePath is set, use it
	if config.ChromePath != "" {
		opts = append(opts, chromedp.ExecPath(config.ChromePath))
	}

	// Create allocator context
	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()

	// Create a new browser context with logging
	browserCtx, browserCancel := chromedp.NewContext(allocCtx, chromedp.WithLogf(log.Printf))
	defer browserCancel()

	// Variable to store the result
	var result string
	var navigationErr error

	// Create a channel to capture page load events
	loadEventFired := make(chan bool, 1)

	// Listen for network events
	chromedp.ListenTarget(browserCtx, func(ev interface{}) {
		switch e := ev.(type) {
		case *network.EventLoadingFinished:
			select {
			case loadEventFired <- true:
			default:
			}
		case *network.EventResponseReceived:
			if e.Response.Status >= 400 {
				log.Printf("Received HTTP status %d for URL: %s", e.Response.Status, url)
			}
		}
	})

	// Set up a navigation task with timeout handling
	navigationTask := chromedp.Tasks{
		// Set up network conditions for faster loading (optional)
		network.Enable(),
		network.SetBypassServiceWorker(true),

		// Navigate to page with error handling
		chromedp.ActionFunc(func(ctx context.Context) error {
			log.Printf("Starting navigation to %s", url)
			err := chromedp.Navigate(url).Do(ctx)
			if err != nil {
				navigationErr = err
				log.Printf("Navigation error: %v", err)
			}
			return nil
		}),

		// Wait for specified time
		chromedp.Sleep(time.Duration(waitSeconds) * time.Second),

		// Attempt to scroll page for dynamic content
		chromedp.ActionFunc(func(ctx context.Context) error {
			log.Printf("Scrolling page to load dynamic content")
			err := chromedp.Evaluate(`
				window.scrollTo({
					top: document.body.scrollHeight,
					behavior: 'smooth',
				});
			`, nil).Do(ctx)

			// Don't fail if scrolling fails
			if err != nil {
				log.Printf("Scrolling error (non-fatal): %v", err)
			}
			return nil
		}),

		// Additional wait after scrolling
		chromedp.Sleep(time.Duration(config.ScrollTimeSec) * time.Second),

		// Extract content
		chromedp.ActionFunc(func(ctx context.Context) error {
			if navigationErr != nil {
				return navigationErr
			}

			log.Printf("Extracting content from page")

			if extractText {
				// Get text content
				err := chromedp.Evaluate(`
					(function() {
						try {
							// Remove script and style elements
							const scripts = document.querySelectorAll('script, style, noscript, iframe');
							for (const script of scripts) {
								script.remove();
							}
							// Get text content
							return document.body.innerText;
						} catch (e) {
							return "Error extracting text: " + e.message;
						}
					})()
				`, &result).Do(ctx)

				if err != nil {
					log.Printf("Error extracting text: %v", err)
					return err
				}
			} else {
				// Get HTML content
				node, err := dom.GetDocument().Do(ctx)
				if err != nil {
					log.Printf("Error getting document: %v", err)
					return err
				}

				result, err = dom.GetOuterHTML().WithNodeID(node.NodeID).Do(ctx)
				if err != nil {
					log.Printf("Error getting HTML: %v", err)
					return err
				}
			}

			return nil
		}),
	}

	// Execute tasks with timeout handling
	log.Printf("Running browser tasks for %s", url)
	if err := chromedp.Run(browserCtx, navigationTask); err != nil {
		log.Printf("Error during browser execution: %v", err)
		return "", fmt.Errorf("browser execution error: %v", err)
	}

	// Check if we got any content
	if result == "" {
		log.Printf("Warning: Empty content retrieved from %s", url)
		return "No content retrieved from the page", nil
	}

	// Truncate if necessary
	if len(result) > config.MaxContentLen {
		truncatedLength := config.MaxContentLen
		result = result[:truncatedLength] + "\n...[content truncated due to length]"
	}

	log.Printf("Successfully retrieved %d characters from %s", len(result), url)
	return result, nil
}

func main() {
	// Create MCP server
	s := server.NewMCPServer(
		"Headless Browser MCP Server", // Server name
		"1.0.0",                       // Version
		server.WithLogging(),
		server.WithRecovery(),
	)

	// Add fetchWebpage tool
	fetchTool := mcp.NewTool("fetch_webpage",
		mcp.WithDescription("Fetch a webpage using a headless browser (includes JavaScript rendering)"),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description("The URL of the webpage to fetch"),
			mcp.Pattern("^https?://.*"),
		),
		mcp.WithNumber("wait_seconds",
			mcp.Description("Seconds to wait for the page to load (default: 10, max: 60)"),
			mcp.Min(1),
			mcp.Max(60),
		),
		mcp.WithBoolean("extract_text",
			mcp.Description("Whether to extract only the visible text (true) or return full HTML (false)"),
		),
	)

	// Add tool handler
	s.AddTool(fetchTool, func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		url, ok := request.Params.Arguments["url"].(string)
		if !ok || url == "" {
			return mcp.NewToolResultError("URL is required"), nil
		}

		// Validate URL
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return mcp.NewToolResultError("URL must start with http:// or https://"), nil
		}

		// Get wait_seconds parameter with default
		waitSeconds := DEFAULT_WAIT_TIME
		if waitSecondsArg, ok := request.Params.Arguments["wait_seconds"].(float64); ok {
			waitSeconds = int(waitSecondsArg)
		}

		// Get extract_text parameter with default
		extractText := false
		if extractTextArg, ok := request.Params.Arguments["extract_text"].(bool); ok {
			extractText = extractTextArg
		}

		log.Printf("Fetching webpage: %s (wait: %ds, extract text: %v)", url, waitSeconds, extractText)

		try := func() (*mcp.CallToolResult, error) {
			content, err := fetchWebpage(url, waitSeconds, extractText)
			if err != nil {
				log.Printf("Error fetching webpage '%s': %v", url, err)
				return mcp.NewToolResultError(fmt.Sprintf("Fetch error: %v", err)), nil
			}

			contentType := "HTML"
			if extractText {
				contentType = "Text"
			}

			return mcp.NewToolResultText(fmt.Sprintf("--- %s Content from %s ---\n\n%s", contentType, url, content)), nil
		}

		// Execute with recovery and retry
		for attempt := 1; attempt <= 2; attempt++ {
			result, err := try()
			if err == nil && !strings.Contains(result.Content[0].(mcp.TextContent).Text, "Fetch error:") {
				return result, nil
			}

			// Only retry once
			if attempt == 1 {
				log.Printf("Retrying fetch for %s", url)
				time.Sleep(2 * time.Second)
			} else {
				if err != nil {
					return mcp.NewToolResultError(fmt.Sprintf("Fetch failed after retry: %v", err)), nil
				} else {
					return result, nil
				}
			}
		}

		// Should never get here due to the return in the loop
		return mcp.NewToolResultError("Unexpected error"), nil
	})

	// Add a resource for fetching webpages as well
	webpageResource := mcp.NewResourceTemplate(
		"web://{url}",
		"Webpage Content",
		mcp.WithTemplateDescription("Returns the content of a webpage rendered using a headless browser"),
		mcp.WithTemplateMIMEType("text/html"),
	)

	s.AddResourceTemplate(webpageResource, func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		uri := request.Params.URI

		// Extract URL from the URI
		if !strings.HasPrefix(uri, "web://") {
			return nil, errors.New("invalid URI format, must start with web://")
		}

		url := uri[6:] // Remove the "web://" prefix

		// Add http:// if missing
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			url = "https://" + url
		}

		// Use the same functionality as the tool
		content, err := fetchWebpage(url, DEFAULT_WAIT_TIME, true)
		if err != nil {
			return nil, err
		}

		return []mcp.ResourceContents{
			mcp.TextResourceContents{
				URI:      request.Params.URI,
				MIMEType: "text/html",
				Text:     content,
			},
		}, nil
	})

	// Log configuration
	config := getBrowserConfig()
	log.Printf("Browser config: Timeout=%ds, WaitTime=%ds, ScrollTime=%ds, Headless=%v",
		config.Timeout, config.DefaultWaitSec, config.ScrollTimeSec, config.Headless)

	// Start the server
	log.Printf("Starting Headless Browser MCP server...")
	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
