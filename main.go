// main.go
//
// This Go app authenticates with the Google Photos API via OAuth2,
// searches for photos using a filter (in this example, the “FAVORITES” feature),
// and downloads each photo that isn’t already present in the specified local folder.
// The code is structured to be easily extensible so you can apply different filters later.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// MediaItem represents a photo item from Google Photos.
type MediaItem struct {
	Id       string `json:"id"`
	BaseUrl  string `json:"baseUrl"`
	Filename string `json:"filename"`
	// Additional fields (e.g., MIME type, metadata) can be added here.
}

// MediaItemsResponse is the response type for a mediaItems search.
type MediaItemsResponse struct {
	MediaItems    []MediaItem `json:"mediaItems"`
	NextPageToken string      `json:"nextPageToken"`
}

// GooglePhotosClient holds an HTTP client to make authorized API calls.
type GooglePhotosClient struct {
	client *http.Client
}

// SearchMediaItems calls the Google Photos API search endpoint with the given filter payload.
// This function handles pagination and returns a slice of MediaItem.
func (gp *GooglePhotosClient) SearchMediaItems(filterPayload map[string]interface{}) ([]MediaItem, error) {
	var allItems []MediaItem
	url := "https://photoslibrary.googleapis.com/v1/mediaItems:search"
	pageToken := ""

	for {
		// Prepare the payload with a page size and optional page token.
		payload := filterPayload
		payload["pageSize"] = 100
		if pageToken != "" {
			payload["pageToken"] = pageToken
		}
		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("error marshaling payload: %v", err)
		}

		req, err := http.NewRequest("POST", url, bytes.NewReader(payloadBytes))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := gp.client.Do(req)
		if err != nil {
			return nil, err
		}

		// Ensure the response body is closed.
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			bodyBytes, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("non-OK HTTP status %d: %s", resp.StatusCode, bodyBytes)
		}

		var itemsResp MediaItemsResponse
		if err := json.NewDecoder(resp.Body).Decode(&itemsResp); err != nil {
			return nil, err
		}

		allItems = append(allItems, itemsResp.MediaItems...)
		if itemsResp.NextPageToken == "" {
			break
		}
		pageToken = itemsResp.NextPageToken

		// Pause briefly to avoid tripping rate limits.
		time.Sleep(100 * time.Millisecond)
	}
	return allItems, nil
}

// DownloadMediaItem downloads a media item from Google Photos by appending "=d" to the baseUrl.
// It saves the file into the provided folder using the media item’s Filename.
// If the file already exists, the download is skipped.
func DownloadMediaItem(item MediaItem, folder string, client *http.Client) error {
	// Append "=d" to the baseUrl to force download in full resolution.
	downloadUrl := item.BaseUrl + "=d"
	filePath := filepath.Join(folder, item.Filename)

	// Check if the file already exists.
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
// It reads the OAuth2 token from token.json (if it exists) or triggers a web auth flow.
func getClient(config *oauth2.Config) (*http.Client, *oauth2.Token) {
	const tokenFile = "token.json"
	tok, err := tokenFromFile(tokenFile)
	if err != nil {
		tok, err = getNewTokenAndSave(config, tokenFile)
		if err != nil {
			log.Fatalf("Unable to retrieve token: %v", err)
		}
	}
	// Check the expiry date of the token: do we need to refresh?
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
	fmt.Printf("Saving token to %s\n", path)
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
		port := ":8080"
		fmt.Println("Starting server on http://localhost" + port)
		if err := http.ListenAndServe(port, nil); err != nil {
			fmt.Println("Error starting server:", err)
			return
		}
	}()

	// Set up the auth code URL to get the state token
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Go to the following link in your browser then type the authorization code:\n%v\n", authURL)

	// Wait for the auth code from the handler
	authCode := <-authCodeChannel // Blocks until `postHandler` sends code

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

	err := r.ParseForm() // Parses request body
	if err != nil {
		http.Error(w, "Error parsing form data", http.StatusBadRequest)
		return
	}

	// Send the authorization code to the channel
	authCodeChannel <- r.FormValue("code")

	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "Authorization code received. You can close this window.")
}

func getNewTokenAndSave(config *oauth2.Config, tokenFile string) (*oauth2.Token, error) {
	tok := getTokenFromWeb(config)
	saveToken(tokenFile, tok)
	return tok, nil
}

func main() {
	// Parse the command-line flag for the target download folder.
	folderPtr := flag.String("folder", "", "Folder location on your PC where photos will be saved")
	flag.Parse()
	if *folderPtr == "" {
		log.Fatal("You must specify a folder location using the -folder flag.")
	}

	// Create the folder if it does not exist.
	if _, err := os.Stat(*folderPtr); os.IsNotExist(err) {
		if err := os.MkdirAll(*folderPtr, os.ModePerm); err != nil {
			log.Fatalf("Unable to create folder %s: %v", *folderPtr, err)
		}
	}

	// Load OAuth2 configuration from credentials.json.
	creds, err := os.ReadFile("credentials.json")
	if err != nil {
		log.Printf("Unable to read credentials file: %v", err)
	}

	const scope = "https://www.googleapis.com/auth/photoslibrary.readonly"
	config, err := google.ConfigFromJSON(creds, scope)

	if err != nil {
		log.Fatalf("Unable to parse credentials file to config: %v", err)
	}
	client, _ := getClient(config)

	photosClient := &GooglePhotosClient{client: client}

	// Define the filter payload.
	// Here we specify the "FAVORITES" feature.
	// To extend this app, you can modify or add filters to this payload.
	filterPayload := map[string]any{
		"filters": map[string]any{
			"featureFilter": map[string]any{
				"includedFeatures": []string{"FAVORITES"},
			},
		},
	}

	fmt.Println("Fetching favorite photos from Google Photos...")
	mediaItems, err := photosClient.SearchMediaItems(filterPayload)
	if err != nil {
		log.Fatalf("Error retrieving favorite photos: %v", err)
	}
	fmt.Printf("Found %d favorite photos.\n", len(mediaItems))

	// Loop through the media items and download each one if it hasn't already been saved.
	for _, item := range mediaItems {
		if err := DownloadMediaItem(item, *folderPtr, client); err != nil {
			fmt.Printf("Error downloading %s: %v\n", item.Filename, err)
		}
	}

	fmt.Println("Finished downloading photos.")
}
