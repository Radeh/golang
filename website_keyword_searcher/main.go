package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

// PageResult stores information for a found page.
type PageResult struct {
	URL         string
	Title       string
	MainText    string
	LastUpdated time.Time
}

// Global HTTP client for reuse.
var httpClient = &http.Client{
	Timeout: 10 * time.Second, // Sensible timeout for HTTP requests.
}

// ByLastUpdated implements sort.Interface for []PageResult based on
// the LastUpdated field, for sorting in descending order (most recent first).
type ByLastUpdated []PageResult

func (a ByLastUpdated) Len() int           { return len(a) }
func (a ByLastUpdated) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a ByLastUpdated) Less(i, j int) bool { return a[j].LastUpdated.Before(a[i].LastUpdated) }

func main() {
	if len(os.Args) < 3 {
		fmt.Println("Usage: go run main.go <startURL> <keyword>")
		os.Exit(1)
	}

	startURLInput := os.Args[1]
	keyword := strings.ToLower(os.Args[2]) // Normalize keyword for case-insensitive search.

	// Validate and parse the start URL.
	parsedStartURL, err := url.Parse(startURLInput)
	if err != nil || (parsedStartURL.Scheme != "http" && parsedStartURL.Scheme != "https") {
		log.Fatalf("Invalid start URL: %s. Please provide a valid http or https URL.", startURLInput)
	}
	// Normalize the start URL (e.g., remove trailing slash for consistent comparison).
	normalizedStartURLStr := strings.TrimRight(parsedStartURL.String(), "/")

	fmt.Printf("Starting crawl from %s to find keyword '%s'\n", normalizedStartURLStr, keyword)

	results, err := crawl(normalizedStartURLStr, keyword)
	if err != nil {
		log.Fatalf("Error during crawl: %v", err)
	}

	// Sort results by LastUpdated (most recent first).
	sort.Sort(ByLastUpdated(results))

	fmt.Printf("\nFound %d pages containing the keyword. Displaying up to 10 most recent:\n", len(results))
	count := 0
	for _, res := range results {
		if count >= 10 { // Limit to 10 results.
			break
		}
		fmt.Printf("\nResult %d:\n", count+1)
		fmt.Printf("URL: %s\n", res.URL)
		fmt.Printf("Title: %s\n", res.Title)
		fmt.Printf("Last Updated: %s\n", res.LastUpdated.Format(time.RFC1123))
		fmt.Printf("Main Text Snippet (first 200 chars): %s...\n", getMainTextSnippet(res.MainText, 200))
		count++
	}
	if count == 0 {
		fmt.Println("No pages found with the keyword.")
	}
}

// crawl manages the website crawling process.
func crawl(startURLStr string, keyword string) ([]PageResult, error) {
	parsedRootURL, _ := url.Parse(startURLStr) // Already validated in main.
	targetDomain := parsedRootURL.Hostname()   // To stay within the same domain.

	queue := make(chan string, 1000)       // Buffered channel for URLs to visit.
	visited := make(map[string]bool)       // Set of URLs that have been added to queue or processed.
	var collectedResults []PageResult      // Slice to store PageResult.

	var mu sync.Mutex                      // Protects `visited` map and `collectedResults` slice.
	var wg sync.WaitGroup                  // To wait for all processing goroutines to finish.

	concurrencyLimit := 5                  // Number of concurrent fetchers.
	activeGoroutines := make(chan struct{}, concurrencyLimit) // Semaphore for limiting concurrency.

	// Initialize with the starting URL.
	mu.Lock()
	visited[startURLStr] = true // Mark start URL as "will be queued".
	mu.Unlock()

	wg.Add(1) // Increment WaitGroup counter for the initial URL.
	queue <- startURLStr

	launcherDone := make(chan struct{})
	// Launcher goroutine: consumes URLs from the queue and spawns worker goroutines.
	go func() {
		defer close(launcherDone)
		for currentURL := range queue { // This loop terminates when 'queue' is closed.
			activeGoroutines <- struct{}{} // Acquire a slot from the semaphore.
			// Spawn a worker goroutine for the currentURL.
			go func(urlToProcess string) {
				processURL(urlToProcess, keyword, targetDomain, queue, &visited, &collectedResults, &mu, &wg, activeGoroutines)
			}(currentURL)
		}
	}()

	// Closer goroutine: waits for all tasks in WaitGroup to complete, then closes the queue.
	go func() {
		wg.Wait()    // Wait for all tasks (initial URL + all discovered URLs) to complete.
		close(queue) // Close the queue, signaling the launcher goroutine to stop.
	}()

	<-launcherDone // Wait for the launcher goroutine to finish (i.e., queue is closed and all items dispatched).
	close(activeGoroutines) // Now safe to close the semaphore channel as no new goroutines will be launched.

	return collectedResults, nil
}

// processURL handles fetching, parsing, and processing a single URL.
func processURL(currentURLStr string, keyword string, targetDomain string,
	queue chan string, visited *map[string]bool, collectedResults *[]PageResult, mu *sync.Mutex,
	wg *sync.WaitGroup, activeGoroutines chan struct{}) {

	defer func() {
		wg.Done()          // Signal that this task (processing currentURLStr) is complete.
		<-activeGoroutines // Release the slot in the concurrency semaphore.
	}()

	fmt.Printf("Fetching %s...\n", currentURLStr)
	req, err := http.NewRequest("GET", currentURLStr, nil)
	if err != nil {
		log.Printf("Error creating request for %s: %v. Skipping.", currentURLStr, err)
		return
	}
	req.Header.Set("User-Agent", "GoKeywordSearcher/1.0") // Set a user agent.

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("Failed to get URL %s: %v. Skipping.", currentURLStr, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Error fetching %s: status code %d. Skipping.", currentURLStr, resp.StatusCode)
		return
	}

	// Attempt to get Last-Modified header for recency.
	lastModifiedHeader := resp.Header.Get("Last-Modified")
	lastUpdated := time.Now() // Default to crawl time if header is missing or unparseable.
	if lm, pErr := time.Parse(time.RFC1123, lastModifiedHeader); pErr == nil {
		lastUpdated = lm
	} else if lm, pErrZ := time.Parse(time.RFC1123Z, lastModifiedHeader); pErrZ == nil { // Try with timezone offset.
		lastUpdated = lm
	}

	doc, err := html.Parse(resp.Body)
	if err != nil {
		log.Printf("Failed to parse HTML from %s: %v. Skipping.", currentURLStr, err)
		return
	}

	title := extractTitle(doc)
	mainText := extractMainText(doc)

	// Check for keyword in title or main text (case-insensitive).
	if strings.Contains(strings.ToLower(title), keyword) || strings.Contains(strings.ToLower(mainText), keyword) {
		mu.Lock()
		*collectedResults = append(*collectedResults, PageResult{
			URL:         currentURLStr,
			Title:       title,
			MainText:    mainText,
			LastUpdated: lastUpdated,
		})
		mu.Unlock()
		fmt.Printf("Keyword '%s' found on %s\n", keyword, currentURLStr)
	}

	// Extract links from the page.
	pageURL, _ := url.Parse(currentURLStr) // Base URL for resolving relative links.
	links := extractLinks(doc, pageURL)    // Returns []*url.URL

	for _, linkURL := range links { // linkURL is *url.URL
		normalizedLinkStr := strings.TrimRight(linkURL.String(), "/")

		// Check if the link is within the target domain.
		if linkURL.Hostname() == targetDomain {
			var shouldAttemptEnqueue bool
			mu.Lock()
			if !(*visited)[normalizedLinkStr] {
				(*visited)[normalizedLinkStr] = true // Mark as "will attempt to queue"
				shouldAttemptEnqueue = true
			}
			mu.Unlock()

			if shouldAttemptEnqueue {
				// This link was not previously marked. It represents a new potential task.
				wg.Add(1) // Optimistically Add for this new task.

				select {
				case queue <- normalizedLinkStr:
					// Successfully enqueued. The wg.Add(1) stands.
					// The launched processURL for this normalizedLinkStr will eventually call wg.Done().
				default:
					// Queue was full. Link was not enqueued by this worker.
					log.Printf("Queue full. Link %s not enqueued by this worker. Reverting visited status.", normalizedLinkStr)

					// Since the task was not actually enqueued, we must decrement the WaitGroup
					// to counteract the optimistic wg.Add(1).
					wg.Done() 
					
					// Revert the visited status so another worker might pick it up later.
					mu.Lock()
					delete(*visited, normalizedLinkStr) // Unmark
					mu.Unlock()
				}
			}
		}
	}
}

// extractLinks recursively finds all <a> href links in an HTML node.
func extractLinks(n *html.Node, baseURL *url.URL) []*url.URL {
	var links []*url.URL
	if n.Type == html.ElementNode && n.Data == "a" {
		for _, a := range n.Attr {
			if a.Key == "href" {
				linkURL, err := baseURL.Parse(a.Val) // Resolve relative URLs against the base.
				if err == nil {
					// Filter for http/https schemes.
					if linkURL.Scheme == "http" || linkURL.Scheme == "https" {
						linkURL.Fragment = "" // Remove fragments as they don't point to new content.
						// Avoid empty or self-referential links post-fragment removal,
						// and links that are identical to the base URL.
						if parsedStr := linkURL.String(); parsedStr != "" && parsedStr != strings.TrimRight(baseURL.String(),"/") {
							links = append(links, linkURL)
						}
					}
				}
				break // Found href, no need to check other attributes of this <a> tag.
			}
		}
	}
	// Recursively check child nodes.
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		links = append(links, extractLinks(c, baseURL)...)
	}
	return links
}

// extractTitle finds the content of the <title> tag.
func extractTitle(n *html.Node) string {
	if n.Type == html.ElementNode && n.Data == "title" {
		if n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
			return strings.TrimSpace(n.FirstChild.Data)
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		title := extractTitle(c)
		if title != "" && title != "No Title Found" { // Propagate found title.
			return title
		}
	}
	return "No Title Found"
}

// extractMainText attempts to get text content, preferring <main> or role="main", then <body>.
func extractMainText(n *html.Node) string {
	var textContent strings.Builder
	var f func(*html.Node)
	f = func(node *html.Node) {
		if node.Type == html.TextNode {
			trimmedData := strings.TrimSpace(node.Data)
			if trimmedData != "" {
				textContent.WriteString(trimmedData)
				textContent.WriteString(" ") // Add space between text nodes.
			}
		}
		// Skip common non-content elements.
		if node.Type == html.ElementNode &&
			(node.Data == "script" || node.Data == "style" || node.Data == "noscript" ||
				node.Data == "header" || node.Data == "footer" || node.Data == "nav" ||
				node.Data == "aside" || node.Data == "form") {
			return
		}
		for c := node.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}

	mainContentNode := findMainContentNode(n)
	if mainContentNode != nil {
		f(mainContentNode)
	} else {
		bodyNode := findBodyNode(n)
		if bodyNode != nil {
			f(bodyNode)
		} else {
			f(n) // Fallback to the whole document if <body> or <main> not found.
		}
	}
	return strings.TrimSpace(textContent.String())
}

// findMainContentNode tries to find HTML5 <main> element or ARIA role="main".
func findMainContentNode(n *html.Node) *html.Node {
	if n.Type == html.ElementNode {
		if n.Data == "main" { // HTML5 <main> element.
			return n
		}
		for _, attr := range n.Attr { // ARIA role="main".
			if attr.Key == "role" && attr.Val == "main" {
				return n
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findMainContentNode(c); found != nil {
			return found
		}
	}
	return nil
}

// findBodyNode finds the <body> element.
func findBodyNode(n *html.Node) *html.Node {
	if n.Type == html.ElementNode && n.Data == "body" {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findBodyNode(c); found != nil {
			return found
		}
	}
	return nil
}

// getMainTextSnippet returns a prefix of the text up to maxLength.
func getMainTextSnippet(text string, maxLength int) string {
	if len(text) <= maxLength {
		return text
	}
	return text[:maxLength]
}