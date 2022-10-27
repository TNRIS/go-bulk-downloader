package main

import (
	"encoding/json"
	"fmt"
	"image/color"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	_ "embed"
	"bytes"
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/widget"
	"fyne.io/fyne/v2/theme"
	"github.com/google/uuid"
)
var wg sync.WaitGroup

var SERVER_LOCATION string = "https://api.tnris.org"

// Keep track of where the user wants to save the files
var save_dir string = ""

// Flag to stop downloading immediately
var stop_now bool = false

// Flag to keep track of whether a download is in progress
var running bool = false

// Keep track of how many items are being downloaded simultaneously
var downloading int = 0

// Keep track of how many items have been downloaded
var downloaded int = 0

// Keeps track of all the responses
var currentDownloads []*http.Response

// out_msg is a Widget that is meant to give feedback for download progress.
var out_msg *widget.Label

// checkBoxes are different categories you can select to limit what types of files to download.
var checkBoxes []*widget.Check

// sel_ctgry stores the selected category when you check a checkbox.
var sel_ctgry string = ""

// inputWidget keeps track of where we will download the files to.
var inputWidget *widget.Label = widget.NewLabel("Browse to folder to download: \n" + save_dir)

// errorWidget shows error messages
var error_msg = canvas.NewText("", color.RGBA{200, 0, 0, 100})

// Embed TNRIS_LOGO.png in binary
//go:embed TNRIS_LOGO.png
var logobytes []byte

// log list of downloads
var logData []string

var pbar *widget.ProgressBar

type RId struct {
	ResourceTypeName string `json:"resource_type_name"`
	ResourceTypeAbbreviation string `json:"resource_type_abbreviation"`
	ResourceTypeCategory string `json:"resource_type_category"`
}

type RIds struct {
	Ids []RId `json:"results"`
}

type ResourceId struct {
	ResourceId string `json:"resource_id"`
	Resource string `json:"resource"`
}

type DataHubItems struct {
	Results []ResourceId `json:"results"`
	Next string `json:"next"`
}

// main sets up the GUI and button actions
func main() {
	myApp := app.New()
	myApp.Settings().SetTheme(theme.DarkTheme())
	//Configure error message
	error_msg.TextStyle = fyne.TextStyle{Bold: true}

	myWindow := myApp.NewWindow("TNRIS DataHub Bulk Download Utility")
	input := widget.NewEntry()
	pbar = widget.NewProgressBar()
	pbar.Hide()

	browseButton := widget.NewButton("Browse", func() {
		error_msg.Text = "" // Clear error messge
		error_msg.Refresh()
		dialog.ShowFolderOpen(func(dir fyne.ListableURI, err error) {
			if err != nil {
				dialog.ShowError(err, myWindow)
				return
			}
			if dir != nil {
				save_dir = dir.Path() // here value of save_dir shall be updated!
				inputWidget.SetText("Browse to folder to download: \n" + save_dir)
			}
		}, myWindow)
	})

	stopButton := widget.NewButton("Stop", func() {
		pbar.Hide()
		error_msg.Text = "" // Clear error messge
		error_msg.Refresh()
		cancelDownloads(true)
		stop_now = true
		running = false
		updateDownloadProgress("")
	})

	getDataButton := widget.NewButton("Get Data", func() {
		error_msg.Text = "" // Clear error messge
		error_msg.Refresh()
		// Run download on a seperate thread from the UI
		if(!running) {
			running = true
			go getData(*input)
		}
	})
	var lb = bytes.NewReader(logobytes)
	var logo *canvas.Image = canvas.NewImageFromReader(lb, "TNRIS_LOGO.png")
	logo.FillMode = canvas.ImageFillContain
	contentUUID := container.New(layout.NewGridLayout(3), container.New(layout.NewVBoxLayout(), widget.NewLabel("Enter a TNRIS DataHub Collection ID: ")), container.New(layout.NewVBoxLayout(),input), logo)
	filterNote := widget.NewLabel("If the collection entered has multiple resource types, filter them here.\nNo filter selection will result in all collection resources downloaded.")
	out_msg = widget.NewLabel("")
	
	stopStartBtn := container.New(layout.NewGridLayout(2), container.New(layout.NewVBoxLayout(), stopButton), container.New(layout.NewVBoxLayout(), getDataButton))
	smallInLab:= container.New(layout.NewVBoxLayout(), inputWidget)
	smallBrowseButton := container.New(layout.NewVBoxLayout(), browseButton)
	
	inputBrowse := container.New(layout.NewGridLayout(3), smallInLab, smallBrowseButton, layout.NewSpacer())
	
	logList := widget.NewList(
		func() int {
			return len(logData)
		},
		func() fyne.CanvasObject {
			return widget.NewLabel("LogData")
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			o.(*widget.Label).SetText(logData[i])
		})
		
	outLog := container.New(layout.NewMaxLayout(), logList)

	item1 := container.New(layout.NewGridLayout(1), contentUUID, inputBrowse, filterNote)
	item2 := container.New(layout.NewMaxLayout(), container.NewVScroll(getCategories()))
	item3 := container.New(layout.NewGridLayout(1), pbar, outLog, error_msg, stopStartBtn)
	allstuff := container.New(layout.NewGridLayout(1), item1, item2, item3)
	myWindow.SetContent(allstuff)
	myWindow.ShowAndRun()
}

// isValidUUID checks a string and determines whether it's a uuid or not
func IsValidUUID(u string) bool {
    _, err := uuid.Parse(u)
    return err == nil
}

// getResp gets a list of DataHubItems from the url sent in
func getResp(getUrl string, type_query string) *DataHubItems{
	if(sel_ctgry != "") {
		getUrl = getUrl + type_query
	}

	resp, err := http.Get(getUrl)
	body, err := io.ReadAll(resp.Body)
	if(err != nil) {
		error_msg.Text = "Error in getResp()"
		error_msg.Refresh()
	}
	defer resp.Body.Close()

	results := &DataHubItems{}
	json.Unmarshal([]byte(string(body)), results)
	
	return results
}

// getData initiates gathering the list of files to download and kicks off the downloads.
func getData(input widget.Entry) {
	// Show progress bar
	pbar.Show()

	stop_now = false
	downloaded = 0

	if(!IsValidUUID(input.Text)) {
		error_msg.Text = "TNRIS Datahub Collection ID is invalid."
		error_msg.Refresh()
		running = false
		return
	}
	if(len([]rune(save_dir)) == 0) {
		error_msg.Text = "No directory has been chosen"
		error_msg.Refresh()
		running = false
		return
	}

	base_url := SERVER_LOCATION + "/api/v1/resources/"
	id_query := "?collection_id="
	type_query := "&resource_type_abbreviation="  + sel_ctgry

	// Build the full url
	getUrl := base_url + id_query + input.Text

	var thing1 *DataHubItems = getResp(getUrl, type_query)
	var allResults []ResourceId
	allResults = thing1.Results

	for (thing1.Next != "") {
		thing1 = getResp(thing1.Next, type_query)
		allResults = append(allResults, thing1.Results...)
	}

	for i := 0; i < len(allResults); i++ {
		// Download 4 items at a time
		if !stop_now && downloading <= 3 {
			downloading++
			wg.Add(1)
			go downloadData(allResults[i].Resource, allResults[i].ResourceId, []int {i+1, len(allResults)})				
		} else if !stop_now {
			downloading++
			wg.Add(1)
			go downloadData(allResults[i].Resource, allResults[i].ResourceId, []int {i+1, len(allResults)})
			wg.Wait()
		} else {
			stop_now = false
			break
		}
	}

	updateDownloadProgress( "0 / " + fmt.Sprint(len(allResults)))
	running = false
}

func updateDownloadProgress(msg string) {
	out_msg.SetText(msg)
}

//downloadData downloads each zip file individually
func downloadData(url string, id string, progress []int) {
	resp, err := http.Get(url)

	fnames := strings.Split(url, "/")
	fname := fnames[len(fnames) - 1]
	logData = append(logData, fname + " Downloading")
	// Check whether any items in abbr_list are true and add them to resource_type_abbreviations
	currentDownloads = append(currentDownloads, resp)
	
	if err != nil {
		error_msg.Text = "err: " + err.Error()
		error_msg.Refresh()
	}

	defer resp.Body.Close()
	fmt.Println("status", resp.Status)
	if resp.StatusCode != 200 {
		log.Println("Statuscode is not 200")
		downloading--
		wg.Done()
		return
	}

	out, err := os.Create(save_dir + "/" + fname)

	if err != nil {
		error_msg.Text = "err: " + err.Error()
		error_msg.Refresh()
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	downloading --
	downloaded ++

	//	update download bar.
	var f float64 = float64(downloaded) / float64(progress[1])
	pbar.SetValue(f)
	updateDownloadProgress(fmt.Sprint(downloaded) + " / " + fmt.Sprint(progress[1]))
	wg.Done()
	logData = append(logData, fname + " Finished. " + fmt.Sprint(downloading) + " In queue. " + fmt.Sprint(downloaded) + " / " + fmt.Sprint(progress[1]))

	if err != nil {
		log.Println("err: " + err.Error())
	}
}

// cancelDownloads loops over the currentDownloads slice (Each item in the currentDownloads slice is a Http.resp)
// then closes them all and deletes their references from the slice.
// It sets the downloading counter to 0 and clears out the currentDownloads slice
// Use this function to stop all downloads in progress and clean up.
func cancelDownloads(reset bool) {
	for i := 0; i < len(currentDownloads); i++ {
		currentDownloads[i].Body.Close()
	}
	currentDownloads = nil
	if(reset) {
		downloading = 0
	}
}

func getCategories() *fyne.Container {
	base_url := SERVER_LOCATION + "/api/v1/resource_types/"

	resp, err := http.Get(base_url)
	body, err := io.ReadAll(resp.Body)
	if(err != nil) {
		error_msg.Text = "Error in getCategories()"
		error_msg.Refresh()
	}

	results := &RIds{}
	json.Unmarshal([]byte(string(body)), results)

	lidarLabel := canvas.NewText("Lidar", color.White)
	lidarLabel.TextStyle = fyne.TextStyle{Bold: true}

	imageryLabel := canvas.NewText("Imagery", color.White)
	imageryLabel.TextStyle = fyne.TextStyle{Bold: true}

	otherLabel := canvas.NewText("Other", color.White)
	otherLabel.TextStyle = fyne.TextStyle{Bold: true}
	lidarBox := container.NewVBox()
	lidarBox.Add(lidarLabel)
	imageryBox := container.NewVBox()

	imageryBox.Add(imageryLabel)
	otherBox := container.NewVBox()
	otherBox.Add(otherLabel)

	for i := 0; i < len(results.Ids); i++ {
		if results.Ids[i].ResourceTypeCategory == "IMAGERY" {
			addCheckToThis(imageryBox, &results.Ids[i])
		} else if results.Ids[i].ResourceTypeCategory == "LIDAR" {
			addCheckToThis(lidarBox, &results.Ids[i])
		} else {
			addCheckToThis(otherBox, &results.Ids[i])
		}
	}

	return container.New(layout.NewGridLayout(3), lidarBox, imageryBox, otherBox)
}


// addCheckToThis takes a fyne.Container, and a RId
// It then parsed the RId and adds a checkbox to the fyne.Container
// It also populates the global variable checkBoxes which stores a slice 
// of references to all the checkboxes.
func addCheckToThis(box *fyne.Container, r *RId) {
	var checkbox *widget.Check
	checkbox = widget.NewCheck(r.ResourceTypeName, func(value bool) {
		if(value) { // If checked (Not unchecked)
			unselect_all_except(checkbox)
			sel_ctgry = r.ResourceTypeAbbreviation
		} else {
			sel_ctgry = ""
		}
	})
	box.Add(checkbox)
	checkBoxes = append(checkBoxes, checkbox) // Store a slice of all the checkboxes to easily uncheck all.
}

// unselect_all_except Unselects all of the checkboxes. Except for the one passed in. 
func unselect_all_except(this_check *widget.Check) {
	for i := 0; i < len(checkBoxes); i++ {
		if(this_check == checkBoxes[i]) {
			continue
		} else {
			checkBoxes[i].SetChecked(false)
		}
	}
}