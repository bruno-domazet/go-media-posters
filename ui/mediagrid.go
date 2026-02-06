package ui

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"log"
	"os"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/data/binding"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/bruno-domazet/go-media-posters/tree"
)

// MediaItem represents a single item in the grid with data binding
type MediaItem struct {
	Node         *tree.Node
	ImageBinding binding.Untyped // Binds to fyne.Resource
	Index        int
}

// MediaGrid displays media as a grid using Fyne's GridWrap widget
type MediaGrid struct {
	widget.BaseWidget
	tree         *tree.Tree
	window       fyne.Window
	items        []*MediaItem
	gridWrap     *widget.GridWrap
	imageCache   map[string]fyne.Resource
	imageCacheMu sync.RWMutex
	loadCtx      context.Context
	loadCancel   context.CancelFunc
	pendingLoads sync.WaitGroup
	onRefresh    func()
	progressBar  *widget.ProgressBarInfinite
	statusLabel  *widget.Label
	mainContent  *fyne.Container
	cols         int
}

// NewMediaGrid creates a new media grid widget
func NewMediaGrid(t *tree.Tree, cols int, win fyne.Window) *MediaGrid {
	ctx, cancel := context.WithCancel(context.Background())

	g := &MediaGrid{
		tree:        t,
		window:      win,
		cols:        cols,
		items:       make([]*MediaItem, 0),
		imageCache:  make(map[string]fyne.Resource),
		loadCtx:     ctx,
		loadCancel:  cancel,
		progressBar: widget.NewProgressBarInfinite(),
		statusLabel: widget.NewLabel("Loading media library..."),
	}

	g.ExtendBaseWidget(g)
	return g
}

// CreateRenderer creates the widget renderer
func (g *MediaGrid) CreateRenderer() fyne.WidgetRenderer {
	// Create loading view
	loadingView := container.NewVBox(
		widget.NewLabel("Building media library..."),
		g.progressBar,
		g.statusLabel,
	)

	g.mainContent = container.NewBorder(nil, nil, nil, nil, loadingView)
	g.Refresh()

	return widget.NewSimpleRenderer(g.mainContent)
}

// UpdateProgress updates the progress display
func (g *MediaGrid) UpdateProgress(files, dirs int64) {
	g.statusLabel.SetText(fmt.Sprintf("Found %d directories and %d files", dirs, files))
}

// Refresh rebuilds the grid from the virtual filesystem
func (g *MediaGrid) Refresh() {
	if g.tree.IsLoading() && len(g.tree.VisibleNodes) == 0 {
		g.progressBar.Start()
		return
	}

	g.progressBar.Stop()

	// Build media items from virtual filesystem
	g.buildMediaItems()

	// Create or update GridWrap widget
	if g.gridWrap == nil {
		g.gridWrap = widget.NewGridWrap(
			func() int {
				return len(g.items)
			},
			func() fyne.CanvasObject {
				// Create template
				img := canvas.NewImageFromResource(theme.MediaVideoIcon())
				img.FillMode = canvas.ImageFillContain
				img.SetMinSize(fyne.NewSize(140, 190))

				label := widget.NewLabel("Template")
				label.Wrapping = fyne.TextWrapWord
				label.Alignment = fyne.TextAlignCenter

				card := container.NewBorder(nil, label, nil, nil, img)
				return card
			},
			func(id widget.GridWrapItemID, obj fyne.CanvasObject) {
				if id >= len(g.items) {
					return
				}

				item := g.items[id]
				card := obj.(*fyne.Container)

				// Update image
				if len(card.Objects) > 0 {
					if img, ok := card.Objects[0].(*canvas.Image); ok {
						// Set current resource
						if val, err := item.ImageBinding.Get(); err == nil {
							if resource, ok := val.(fyne.Resource); ok {
								img.Resource = resource
								img.Refresh()
							}
						}
					}
				}

				// Update label (last object in border container)
				if label, ok := card.Objects[len(card.Objects)-1].(*widget.Label); ok {
					label.SetText(item.Node.Name)
				}
			},
		)

		// Handle selection
		g.gridWrap.OnSelected = func(id widget.GridWrapItemID) {
			if id >= len(g.items) {
				return
			}

			item := g.items[id]
			g.tree.SelectedIdx = int(id)

			if item.Node.IsDir {
				g.tree.Enter()
				g.Refresh()
				if g.onRefresh != nil {
					g.onRefresh()
				}
			} else {
				log.Printf("Selected: %s", item.Node.Path)
			}
		}
	}

	// Update GridWrap data
	g.gridWrap.Refresh()

	// Select current item
	if g.tree.SelectedIdx >= 0 && g.tree.SelectedIdx < len(g.items) {
		g.gridWrap.Select(widget.GridWrapItemID(g.tree.SelectedIdx))
		g.gridWrap.ScrollTo(widget.GridWrapItemID(g.tree.SelectedIdx))
	}

	// Update main content
	pathLabel := widget.NewLabel("📁 " + g.tree.CurrentDir.Path)
	pathLabel.Wrapping = fyne.TextWrapWord

	content := container.NewBorder(
		pathLabel,
		nil, nil, nil,
		g.gridWrap,
	)

	g.mainContent.Objects = []fyne.CanvasObject{content}
	g.mainContent.Refresh()

	log.Printf("Rendered grid with %d items", len(g.items))
}

// buildMediaItems creates MediaItem wrappers for visible nodes
func (g *MediaGrid) buildMediaItems() {
	nodes := g.tree.VisibleNodes
	g.items = make([]*MediaItem, len(nodes))

	for i, node := range nodes {
		item := &MediaItem{
			Node:         node,
			ImageBinding: binding.NewUntyped(),
			Index:        i,
		}

		// Set placeholder initially
		item.ImageBinding.Set(g.getPlaceholderResource(node))

		g.items[i] = item

		// Start async load if poster exists
		if node.PosterPath != "" {
			go g.loadImageAsync(node.PosterPath, item)
		}
	}
}

// getPlaceholderResource returns appropriate placeholder for node type
func (g *MediaGrid) getPlaceholderResource(node *tree.Node) fyne.Resource {
	if node.IsDir {
		return theme.FolderIcon()
	} else if node.IsVideo {
		return theme.MediaVideoIcon()
	}
	return theme.FileIcon()
}

// loadImageAsync loads an image and updates binding when ready
func (g *MediaGrid) loadImageAsync(posterPath string, item *MediaItem) {
	g.pendingLoads.Add(1)
	defer g.pendingLoads.Done()

	// Check if cancelled
	select {
	case <-g.loadCtx.Done():
		return
	default:
	}

	// Check cache first
	g.imageCacheMu.RLock()
	if cached, exists := g.imageCache[posterPath]; exists {
		g.imageCacheMu.RUnlock()
		item.ImageBinding.Set(cached)
		return
	}
	g.imageCacheMu.RUnlock()

	// Load from disk
	data, err := os.ReadFile(posterPath)
	if err != nil {
		return
	}

	// Validate format
	_, _, err = image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return
	}

	// Check cancelled
	select {
	case <-g.loadCtx.Done():
		return
	default:
	}

	// Create resource
	resource := fyne.NewStaticResource(posterPath, data)

	// Cache it
	g.imageCacheMu.Lock()
	g.imageCache[posterPath] = resource
	g.imageCacheMu.Unlock()

	// Update binding (this triggers UI update automatically)
	item.ImageBinding.Set(resource)

	// Refresh the grid item to show the new image (must be on UI thread)
	if g.gridWrap != nil {
		itemID := widget.GridWrapItemID(item.Index)
		fyne.Do(func() {
			g.gridWrap.RefreshItem(itemID)
		})
	}
}

// TypedKey handles keyboard navigation
func (g *MediaGrid) TypedKey(key *fyne.KeyEvent) {
	if g.gridWrap == nil || len(g.items) == 0 {
		return
	}

	currentID := widget.GridWrapItemID(g.tree.SelectedIdx)
	var newID widget.GridWrapItemID

	// Get actual column count from GridWrap
	cols := g.gridWrap.ColumnCount()

	switch key.Name {
	case fyne.KeyUp:
		if currentID >= cols {
			newID = currentID - cols
		} else {
			return
		}
	case fyne.KeyDown:
		if currentID+cols < len(g.items) {
			newID = currentID + cols
		} else {
			return
		}
	case fyne.KeyLeft:
		if currentID > 0 {
			newID = currentID - 1
		} else {
			return
		}
	case fyne.KeyRight:
		if currentID < len(g.items)-1 {
			newID = currentID + 1
		} else {
			return
		}
	case fyne.KeyReturn, fyne.KeyEnter:
		// Trigger selection
		if g.gridWrap.OnSelected != nil && currentID < len(g.items) {
			g.gridWrap.OnSelected(currentID)
		}
		return
	case fyne.KeyBackspace:
		g.tree.GoUp()
		g.Refresh()
		return
	default:
		return
	}

	// Update selection
	g.tree.SelectedIdx = int(newID)
	g.gridWrap.Select(newID)
	g.gridWrap.ScrollTo(newID)
}

// TypedRune implements Focusable
func (g *MediaGrid) TypedRune(r rune) {}

// SetOnRefresh sets callback for refresh events
func (g *MediaGrid) SetOnRefresh(callback func()) {
	g.onRefresh = callback
}
