// main.go
//
// This Go app provides a web interface for selecting and downloading photos from Google Photos
// using the Google Photos Picker API.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const sessionURL = "https://photospicker.googleapis.com/v1/sessions"
const mediaItemsURL = "https://photospicker.googleapis.com/v1/mediaItems"

type PollingConfig struct {
	PollInterval string `json:"pollInterval"`
	TimeoutIn    string `json:"timeoutIn"`
}

type PickingSession struct {
	ID            string        `json:"id"`
	MediaItemsSet bool          `json:"mediaItemsSet"`
	PickerURI     string        `json:"pickerUri"`
	PollingConfig PollingConfig `json:"pollingConfig"`
}

type MediaFile struct {
	BaseUrl  string `json:"baseUrl"`
	Filename string `json:"filename"`
}

type MediaType string

const (
	MediaTypePhoto           MediaType = "PHOTO"
	MediaTypeVideo           MediaType = "VIDEO"
	MediaTypeTypeUnspecified MediaType = "TYPE_UNSPECIFIED"
)

type PickedMediaItem struct {
	Id         string    `json:"id"`
	CreateTime string    `json:"createTime"`
	Type       MediaType `json:"type"`
	MediaFile  MediaFile `json:"mediaFile"`
}

type MediaItemsList struct {
	MediaItems    []PickedMediaItem `json:"mediaItems"`
	NextPageToken string            `json:"nextPageToken"`
}

type DownloadableMediaItems struct {
	MediaItems []PickedMediaItem
}

// DownloadMediaItem downloads a media item from Google Photos by appending "=d" to the baseUrl.
func DownloadMediaItem(item MediaFile, folder string, client *http.Client) error {
	downloadUrl := item.BaseUrl + "=d"
	filePath := filepath.Join(folder, item.Filename)

	if _, err := os.Stat(filePath); err == nil {
		fmt.Printf("File %s already exists, skipping download.\n", item.Filename)
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	resp, err := client.Get(downloadUrl)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to download file %s, HTTP status %d", item.Filename, resp.StatusCode)
	}

	out, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return err
	}

	fmt.Printf("Downloaded: %s\n", item.Filename)
	return nil
}

// getClient retrieves an authenticated HTTP client using OAuth2 credentials.
func getClient(config *oauth2.Config) (*http.Client, *oauth2.Token) {
	const tokenFile = "token.json"
	tok, err := tokenFromFile(tokenFile)
	if err != nil {
		tok, err = getNewTokenAndSave(config, tokenFile)
		if err != nil {
			log.Fatalf("Unable to retrieve token: %v", err)
		}
	}
	if tok.Expiry.Before(time.Now()) {
		tok, err = getNewTokenAndSave(config, tokenFile)
		if err != nil {
			log.Fatalf("Unable to retrieve token: %v", err)
		}
	}
	return config.Client(context.Background(), tok), tok
}

// tokenFromFile retrieves an OAuth2 token from a file.
func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

// saveToken writes the OAuth2 token to a specified file path.
func saveToken(path string, token *oauth2.Token) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatalf("Unable to cache token: %v", err)
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}

var authCodeChannel = make(chan string)

// getTokenFromWeb initiates an OAuth2 web flow to retrieve a new token.
func getTokenFromWeb(config *oauth2.Config) *oauth2.Token {
	// Start a web server
	http.HandleFunc("/", postHandler)

	go func() {
		port := ":8080" // Different port for auth callback
		fmt.Println("Starting OAuth callback server on http://localhost" + port)
		if err := http.ListenAndServe(port, nil); err != nil {
			fmt.Println("Error starting server:", err)
			return
		}
	}()

	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Go to the following link in your browser then type the authorization code:\n%v\n", authURL)

	authCode := <-authCodeChannel

	tok, err := config.Exchange(context.Background(), authCode)
	if err != nil {
		log.Fatalf("Unable to retrieve token from web: %v", err)
	}
	return tok
}

func postHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		return
	}

	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Error parsing form data", http.StatusBadRequest)
		return
	}

	authCodeChannel <- r.FormValue("code")

	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "Authorization code received. You can close this window.")
}

func getNewTokenAndSave(config *oauth2.Config, tokenFile string) (*oauth2.Token, error) {
	tok := getTokenFromWeb(config)
	saveToken(tokenFile, tok)
	return tok, nil
}

func newSession(client *http.Client) (PickingSession, error) {

	resp, err := client.Post(sessionURL, "application/json", nil)

	if err != nil {
		log.Fatalf("Failed to create session: %v", err)
	}

	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return PickingSession{}, fmt.Errorf("failed to create session: status %d", resp.StatusCode)
	}

	var sessionResult PickingSession
	if err := json.NewDecoder(resp.Body).Decode(&sessionResult); err != nil {
		return PickingSession{}, fmt.Errorf("failed to decode session response: %v", err)
	}
	return sessionResult, nil

}

func getMediaItemsFromFirstPage(client *http.Client, sessionID string) (MediaItemsList, error) {
	mediaItemsURL, err := url.Parse(mediaItemsURL)
	if err != nil {
		return MediaItemsList{}, fmt.Errorf("failed to parse media items URL: %v", err)
	}
	mediaItemsQuery := mediaItemsURL.Query()
	mediaItemsQuery.Add("sessionId", sessionID)
	mediaItemsQuery.Add("pageSize", "100")
	mediaItemsURL.RawQuery = mediaItemsQuery.Encode()

	resp, err := client.Get(mediaItemsURL.String())
	if err != nil {
		return MediaItemsList{}, fmt.Errorf("failed to get media items: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return MediaItemsList{}, fmt.Errorf("failed to fetch media items: status %d", resp.StatusCode)
	}

	var firstPageItems MediaItemsList
	if err := json.NewDecoder(resp.Body).Decode(&firstPageItems); err != nil {
		return MediaItemsList{}, fmt.Errorf("failed to decode media items response: %v", err)
	}
	return firstPageItems, nil
}

func getMediaItemsFromPageURL(client *http.Client, sessionID string, pageToken string) (MediaItemsList, error) {
	mediaItemsURL, err := url.Parse(mediaItemsURL)
	if err != nil {
		return MediaItemsList{}, fmt.Errorf("failed to parse media items URL: %v", err)
	}
	mediaItemsQuery := mediaItemsURL.Query()
	mediaItemsQuery.Add("sessionId", sessionID)
	mediaItemsQuery.Add("pageSize", "100")
	mediaItemsQuery.Add("pageToken", pageToken)
	mediaItemsURL.RawQuery = mediaItemsQuery.Encode()

	resp, err := client.Get(mediaItemsURL.String())
	if err != nil {
		return MediaItemsList{}, fmt.Errorf("failed to get media items from page URL: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return MediaItemsList{}, fmt.Errorf("failed to fetch media items: status %d", resp.StatusCode)
	}
	var pageItems MediaItemsList
	if err := json.NewDecoder(resp.Body).Decode(&pageItems); err != nil {
		return MediaItemsList{}, fmt.Errorf("failed to decode media items response: %v", err)
	}
	return pageItems, nil
}

func fetchSelectedMediaItems(client *http.Client, sessionID string) (DownloadableMediaItems, error) {
	var downloadableItems DownloadableMediaItems

	firstPageList, err := getMediaItemsFromFirstPage(client, sessionID)
	if err != nil {
		return DownloadableMediaItems{}, fmt.Errorf("failed to fetch first page media items: %v", err)
	}
	downloadableItems.MediaItems = firstPageList.MediaItems

	// Next page token has been returned
	nextPageToken := firstPageList.NextPageToken
	for nextPageToken != "" {
		pageList, err := getMediaItemsFromPageURL(client, sessionID, nextPageToken)
		if err != nil {
			return DownloadableMediaItems{}, fmt.Errorf("failed to fetch next page media items: %v", err)
		}
		downloadableItems.MediaItems = append(downloadableItems.MediaItems, pageList.MediaItems...)
		nextPageToken = pageList.NextPageToken
	}

	return downloadableItems, nil
}

// parseDuration converts a duration string like "30s" or "1m" to time.Duration
func parseDuration(duration string) (time.Duration, error) {
	// Remove any quotes if present
	duration = strings.Trim(duration, "\"")
	return time.ParseDuration(duration)
}

func pollForCompleteSession(client *http.Client, sessionID string) (bool, error) {
	sessionCheckURL := fmt.Sprintf("%s/%s", sessionURL, sessionID)
	resp, err := client.Get(sessionCheckURL)
	if err != nil {
		return false, fmt.Errorf("failed to check session: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("failed to check session: status %d", resp.StatusCode)
	}

	var sessionResult PickingSession
	if err := json.NewDecoder(resp.Body).Decode(&sessionResult); err != nil {
		return false, fmt.Errorf("failed to decode session response: %v", err)
	}
	return sessionResult.MediaItemsSet, nil
}

// waitForSessionComplete polls the session until it's complete or times out
func waitForSessionComplete(client *http.Client, session PickingSession) (DownloadableMediaItems, error) {
	// Parse the polling interval
	interval, err := parseDuration(session.PollingConfig.PollInterval)
	if err != nil {
		return DownloadableMediaItems{}, fmt.Errorf("invalid polling interval: %v", err)
	}

	// Parse the timeout
	timeout, err := parseDuration(session.PollingConfig.TimeoutIn)
	if err != nil {
		return DownloadableMediaItems{}, fmt.Errorf("invalid timeout: %v", err)
	}

	// Create a timer for the overall timeout
	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()

	// Create a ticker for polling at the specified interval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Start polling
	for {
		select {
		case <-timeoutTimer.C:
			return DownloadableMediaItems{}, fmt.Errorf("session timed out after %v", timeout)

		case <-ticker.C:
			complete, err := pollForCompleteSession(client, session.ID)
			if err != nil {
				return DownloadableMediaItems{}, fmt.Errorf("polling failed: %v", err)
			}

			if complete {
				// Fetch the selected media items
				mediaItems, err := fetchSelectedMediaItems(client, session.ID)
				if err != nil {
					return DownloadableMediaItems{}, fmt.Errorf("failed to fetch selected media items: %v", err)
				}

				return mediaItems, nil
			}
		}
	}
}

func downloadItems(client *http.Client, items DownloadableMediaItems, folder string) {
	for _, item := range items.MediaItems {
		if err := DownloadMediaItem(item.MediaFile, folder, client); err != nil {
			fmt.Printf("Error downloading %s: %v\n", item.MediaFile.Filename, err)
		}
	}
}

func main() {
	folderPtr := flag.String("folder", "", "Folder location on your PC where photos will be saved")
	flag.Parse()

	if *folderPtr == "" {
		log.Fatal("You must specify a folder location using the -folder flag.")
	}

	downloadPath := *folderPtr

	if _, err := os.Stat(downloadPath); os.IsNotExist(err) {
		if err := os.MkdirAll(downloadPath, os.ModePerm); err != nil {
			log.Fatalf("Unable to create folder %s: %v", downloadPath, err)
		}
	}

	creds, err := os.ReadFile("credentials.json")
	if err != nil {
		log.Fatalf("Unable to read credentials file: %v", err)
	}

	const scope = "https://www.googleapis.com/auth/photospicker.mediaitems.readonly https://www.googleapis.com/auth/userinfo.profile"
	config, err := google.ConfigFromJSON(creds, scope)
	if err != nil {
		log.Fatalf("Unable to parse credentials file to config: %v", err)
	}

	client, _ := getClient(config)

	// Create a google photos picker session
	pickingSession, err := newSession(client)
	if err != nil {
		log.Fatalf("Failed to initialise photos picker session: %v", err)
	}

	// Print the picker URL so the user can open it in their browser
	fmt.Printf("\nOpen the following URL in your browser to select photos:\n%s\n", pickingSession.PickerURI)
	fmt.Printf("\nWaiting for photo selection (timeout: %s, polling every %s)...\n",
		pickingSession.PollingConfig.TimeoutIn,
		pickingSession.PollingConfig.PollInterval)

	// Wait for the user to complete their photo selection
	downloadableItems, err := waitForSessionComplete(client, pickingSession)
	if err != nil {
		log.Fatalf("Failed while waiting for photo selection: %v", err)
	}

	// Download the downloadable items
	downloadItems(client, downloadableItems, downloadPath)
}
