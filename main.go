package main

import (
	"log"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"github.com/bruno-domazet/go-media-posters/tree"
	"github.com/bruno-domazet/go-media-posters/ui"
)

func main() {
	a := app.New()
	win := a.NewWindow("File Tree Browser")
	win.Resize(fyne.NewSize(1000, 700))

	// Configuration
	rootPath := "/Users/brdo/repos/private/go-media-posters/assets"
	cols := 4

	// Create filter to skip unwanted files
	filter := tree.DefaultFilter()

	var mediaGrid *ui.MediaGrid

	// Progress callback
	onProgress := func(files, dirs int64) {
		if mediaGrid != nil {
			mediaGrid.UpdateProgress(files, dirs)
		}
	}

	// Completion callback
	onComplete := func() {
		log.Println("Tree loading complete!")
		if mediaGrid != nil {
			mediaGrid.Refresh()
		}
	}

	// Load tree asynchronously with filter
	fileTree, err := tree.LoadAsync(rootPath, filter, onProgress, onComplete)
	if err != nil {
		log.Fatalf("Failed to load tree: %v", err)
	}

	// Create UI with new MediaGrid
	mediaGrid = ui.NewMediaGrid(fileTree, cols, win)

	// Create container with instructions
	content := container.NewBorder(
		widget.NewLabel("Arrow Keys: navigate | Enter: open | Backspace: go up | Click: select"),
		nil, nil, nil,
		mediaGrid,
	)

	win.SetContent(content)

	// Set up keyboard shortcuts
	win.Canvas().SetOnTypedKey(func(key *fyne.KeyEvent) {
		mediaGrid.TypedKey(key)
	})

	win.ShowAndRun()
}
