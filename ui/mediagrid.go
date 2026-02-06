package ui

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"log"
	"os"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/data/binding"
	"fyne.io/fyne/v2/layout"
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

// MediaGrid displays media as a grid of poster images
type MediaGrid struct {
	widget.BaseWidget
	tree         *tree.Tree
	window       fyne.Window
	cols         int
	rowHeight    float32
	colWidth     float32
	items        []*MediaItem
	visibleStart int
	visibleEnd   int
	batchSize    int
	container    *fyne.Container
	scrollBar    *container.Scroll
	visibleCards []fyne.CanvasObject // Cache rendered cards
	imageCache   map[string]fyne.Resource
	imageCacheMu sync.RWMutex
	selectedIdx  int
	loadCtx      context.Context
	loadCancel   context.CancelFunc
	pendingLoads sync.WaitGroup
	renderCtx    context.Context
	renderCancel context.CancelFunc
	isRendering  bool
	renderMu     sync.Mutex
	onRefresh    func()
	progressBar  *widget.ProgressBarInfinite
	statusLabel  *widget.Label
	mainContent  *fyne.Container
}

// NewMediaGrid creates a new media grid widget
func NewMediaGrid(t *tree.Tree, cols int, win fyne.Window) *MediaGrid {
	ctx, cancel := context.WithCancel(context.Background())
	renderCtx, renderCancel := context.WithCancel(context.Background())

	g := &MediaGrid{
		tree:         t,
		window:       win,
		cols:         cols,
		rowHeight:    200,
		colWidth:     150,
		batchSize:    50, // Render in smaller batches
		items:        make([]*MediaItem, 0),
		imageCache:   make(map[string]fyne.Resource),
		loadCtx:      ctx,
		loadCancel:   cancel,
		renderCtx:    renderCtx,
		renderCancel: renderCancel,
		progressBar:  widget.NewProgressBarInfinite(),
		statusLabel:  widget.NewLabel("Loading media library..."),
	}

	g.ExtendBaseWidget(g)
	return g
}

// CreateRenderer creates the widget renderer
func (g *MediaGrid) CreateRenderer() fyne.WidgetRenderer {
	g.container = container.NewVBox()

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

	// Cancel any ongoing progressive rendering
	if g.renderCancel != nil {
		g.renderCancel()
	}
	g.renderCtx, g.renderCancel = context.WithCancel(context.Background())

	// Build media items from virtual filesystem (tree in memory)
	g.buildMediaItems()

	// Render initial batch
	g.renderVisibleBatch()

	log.Printf("Rendered initial batch %d-%d of %d items",
		g.visibleStart, g.visibleEnd, len(g.items))

	// Start progressive rendering in background
	go g.progressiveRender()
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
}

// renderVisibleBatch renders items in the current window
func (g *MediaGrid) renderVisibleBatch() {
	g.container.Objects = nil

	// Add path header
	pathLabel := widget.NewLabel("📁 " + g.tree.CurrentDir.Path)
	pathLabel.Wrapping = fyne.TextWrapWord
	g.container.Add(pathLabel)

	// Calculate visible range
	totalItems := len(g.items)
	g.visibleStart = 0
	g.visibleEnd = totalItems
	if totalItems > g.batchSize {
		g.visibleEnd = g.batchSize

		infoLabel := widget.NewLabel(
			fmt.Sprintf("Showing %d-%d of %d items (scroll for more)",
				g.visibleStart+1, g.visibleEnd, totalItems))
		g.container.Add(infoLabel)
	}

	// Create grid container for media items
	gridContainer := container.New(layout.NewGridWrapLayout(fyne.NewSize(g.colWidth, g.rowHeight)))

	// Add visible items
	visibleCards := make([]fyne.CanvasObject, 0)
	for i := g.visibleStart; i < g.visibleEnd && i < len(g.items); i++ {
		item := g.items[i]
		mediaCard := g.createMediaCard(item)
		gridContainer.Add(mediaCard)
		visibleCards = append(visibleCards, mediaCard)
	}
	g.visibleCards = visibleCards

	g.container.Add(gridContainer)

	// Update main content with scroll container
	if g.scrollBar == nil {
		g.scrollBar = container.NewVScroll(g.container)
		g.mainContent.Objects = []fyne.CanvasObject{g.scrollBar}
	} else {
		g.scrollBar.Content = g.container
		g.scrollBar.Refresh()
	}
	g.mainContent.Refresh()
}

// createMediaCard creates a card for a media item with data-bound image
func (g *MediaGrid) createMediaCard(item *MediaItem) fyne.CanvasObject {
	// Create image canvas
	img := canvas.NewImageFromResource(theme.MediaVideoIcon())
	img.FillMode = canvas.ImageFillContain
	img.SetMinSize(fyne.NewSize(g.colWidth-10, g.rowHeight-40))

	// Bind image to data binding
	item.ImageBinding.AddListener(binding.NewDataListener(func() {
		if val, err := item.ImageBinding.Get(); err == nil {
			if resource, ok := val.(fyne.Resource); ok {
				img.Resource = resource
				img.Refresh()
			}
		}
	}))

	// Initial value
	if val, err := item.ImageBinding.Get(); err == nil {
		if resource, ok := val.(fyne.Resource); ok {
			img.Resource = resource
		}
	}

	// Create label
	label := widget.NewLabel(item.Node.Name)
	label.Wrapping = fyne.TextWrapWord
	label.Alignment = fyne.TextAlignCenter

	// Create card container
	card := container.NewBorder(
		nil,
		label,
		nil,
		nil,
		img,
	)

	// Make it tappable
	tappable := newTappableContainer(card, func() {
		g.onItemTapped(item)
	})

	// Highlight if selected
	if item.Index == g.tree.SelectedIdx {
		tappable.(*tappableContainer).selected = true
	}

	return tappable
}

// progressiveRender renders remaining items in batches asynchronously
func (g *MediaGrid) progressiveRender() {
	g.renderMu.Lock()
	if g.isRendering {
		g.renderMu.Unlock()
		return
	}
	g.isRendering = true
	g.renderMu.Unlock()

	defer func() {
		g.renderMu.Lock()
		g.isRendering = false
		g.renderMu.Unlock()
	}()

	totalItems := len(g.items)
	if g.visibleEnd >= totalItems {
		return
	}

	// Get the grid container
	var gridContainer *fyne.Container
	if len(g.container.Objects) > 0 {
		if c, ok := g.container.Objects[len(g.container.Objects)-1].(*fyne.Container); ok {
			gridContainer = c
		}
	}

	if gridContainer == nil {
		return
	}

	// Render remaining items in batches with small delays
	for g.visibleEnd < totalItems {
		// Check if cancelled
		select {
		case <-g.renderCtx.Done():
			return
		default:
		}

		// Calculate next batch
		batchStart := g.visibleEnd
		batchEnd := min(batchStart+g.batchSize, totalItems)

		// Update visible end immediately (before async UI update)
		g.visibleEnd = batchEnd

		// Batch all UI operations together
		fyne.Do(func() {
			// Add batch items
			for i := batchStart; i < batchEnd; i++ {
				item := g.items[i]
				mediaCard := g.createMediaCard(item)
				gridContainer.Add(mediaCard)
				g.visibleCards = append(g.visibleCards, mediaCard)
			}

			// Update info label
			if len(g.container.Objects) >= 2 {
				if label, ok := g.container.Objects[1].(*widget.Label); ok {
					if batchEnd < totalItems {
						label.SetText(fmt.Sprintf("Showing %d-%d of %d items (loading...)",
							g.visibleStart+1, batchEnd, totalItems))
					} else {
						label.SetText(fmt.Sprintf("Showing all %d items", totalItems))
					}
				}
			}

			// Refresh grid
			gridContainer.Refresh()
			g.scrollBar.Content.Refresh()
		})

		log.Printf("Progressive render: added batch %d-%d (total: %d)", batchStart, batchEnd, totalItems)

		// Small delay to avoid overwhelming UI thread
		if batchEnd < totalItems {
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-g.renderCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
				// Continue to next batch
			}
		}
	}
}

// updateSelection updates only the selection highlight without rebuilding the grid
func (g *MediaGrid) updateSelection(oldIdx, newIdx int) {
	// Update within visible range
	if oldIdx >= g.visibleStart && oldIdx < g.visibleEnd {
		cardIdx := oldIdx - g.visibleStart
		if cardIdx >= 0 && cardIdx < len(g.visibleCards) {
			if card, ok := g.visibleCards[cardIdx].(*tappableContainer); ok {
				card.selected = false
				card.Refresh()
			}
		}
	}

	if newIdx >= g.visibleStart && newIdx < g.visibleEnd {
		cardIdx := newIdx - g.visibleStart
		if cardIdx >= 0 && cardIdx < len(g.visibleCards) {
			if card, ok := g.visibleCards[cardIdx].(*tappableContainer); ok {
				card.selected = true
				card.Refresh()
			}
		}
	}

	// Scroll to keep selected item visible (items will render progressively)
	g.scrollToSelection(newIdx)
}

// scrollToSelection ensures the selected item is visible in the scroll view
func (g *MediaGrid) scrollToSelection(idx int) {
	if g.scrollBar == nil || idx < 0 || idx >= len(g.items) {
		return
	}

	// Only scroll if item is actually rendered
	if idx >= g.visibleEnd {
		return // Item not rendered yet
	}

	// Calculate the actual rendered position based on visible cards
	cardIdx := idx - g.visibleStart
	if cardIdx < 0 || cardIdx >= len(g.visibleCards) {
		return
	}

	// Calculate position based on rendered rows
	row := idx / g.cols
	rowY := float32(row) * g.rowHeight

	if g.scrollBar.Content == nil {
		return
	}

	viewHeight := g.scrollBar.Size().Height
	currentOffset := g.scrollBar.Offset.Y

	// Define visible region with padding
	topThreshold := currentOffset + 50                               // 50px from top
	bottomThreshold := currentOffset + viewHeight - g.rowHeight - 50 // 50px from bottom

	// Check if item is outside comfortable viewing area
	needsScroll := false
	var newOffset float32

	if rowY < topThreshold {
		// Item is too close to top or above - center it in upper portion
		newOffset = rowY - g.rowHeight
		if newOffset < 0 {
			newOffset = 0
		}
		needsScroll = true
	} else if rowY > bottomThreshold {
		// Item is too close to bottom or below - center it in lower portion
		newOffset = rowY - viewHeight + g.rowHeight*2
		if newOffset < 0 {
			newOffset = 0
		}
		needsScroll = true
	}

	if needsScroll {
		g.scrollBar.ScrollToOffset(fyne.NewPos(0, newOffset))
	}
}

// tappableContainer wraps a container to make it tappable
type tappableContainer struct {
	widget.BaseWidget
	content  fyne.CanvasObject
	onTapped func()
	selected bool
}

func newTappableContainer(content fyne.CanvasObject, onTapped func()) fyne.CanvasObject {
	t := &tappableContainer{
		content:  content,
		onTapped: onTapped,
	}
	t.ExtendBaseWidget(t)
	return t
}

func (t *tappableContainer) CreateRenderer() fyne.WidgetRenderer {
	bg := canvas.NewRectangle(color.Transparent)
	if t.selected {
		bg.FillColor = theme.Color(theme.ColorNamePrimary)
		bg.CornerRadius = 4
	}

	return &tappableRenderer{
		container: t,
		bg:        bg,
		objects:   []fyne.CanvasObject{bg, t.content},
	}
}

func (t *tappableContainer) Tapped(*fyne.PointEvent) {
	if t.onTapped != nil {
		t.onTapped()
	}
}

type tappableRenderer struct {
	container *tappableContainer
	bg        *canvas.Rectangle
	objects   []fyne.CanvasObject
}

func (r *tappableRenderer) Layout(size fyne.Size) {
	r.bg.Resize(size)
	r.container.content.Resize(size)
}

func (r *tappableRenderer) MinSize() fyne.Size {
	return r.container.content.MinSize()
}

func (r *tappableRenderer) Refresh() {
	if r.container.selected {
		r.bg.FillColor = theme.Color(theme.ColorNamePrimary)
	} else {
		r.bg.FillColor = color.Transparent
	}
	r.bg.Refresh()
	canvas.Refresh(r.container.content)
}

func (r *tappableRenderer) Objects() []fyne.CanvasObject {
	return r.objects
}

func (r *tappableRenderer) Destroy() {}

// onItemTapped handles item selection and navigation
func (g *MediaGrid) onItemTapped(item *MediaItem) {
	g.tree.SelectedIdx = item.Index

	if item.Node.IsDir {
		// Navigate into directory (using virtual filesystem)
		g.tree.Enter()
		g.Refresh()
		if g.onRefresh != nil {
			g.onRefresh()
		}
	} else {
		log.Printf("Selected: %s", item.Node.Path)
	}
}

// TypedKey handles keyboard navigation
func (g *MediaGrid) TypedKey(key *fyne.KeyEvent) {
	oldIdx := g.tree.SelectedIdx

	switch key.Name {
	case fyne.KeyUp:
		g.tree.NavigateUp(g.cols)
	case fyne.KeyDown:
		g.tree.NavigateDown(g.cols)
	case fyne.KeyLeft:
		g.tree.NavigateLeft()
	case fyne.KeyRight:
		g.tree.NavigateRight()
	case fyne.KeyReturn, fyne.KeyEnter:
		if g.tree.SelectedIdx >= 0 && g.tree.SelectedIdx < len(g.items) {
			g.onItemTapped(g.items[g.tree.SelectedIdx])
		}
		return
	case fyne.KeyBackspace:
		g.tree.GoUp()
		g.Refresh()
		return
	}

	// Update selection visually if changed
	if oldIdx != g.tree.SelectedIdx {
		g.updateSelection(oldIdx, g.tree.SelectedIdx)
	}
}

// TypedRune implements Focusable
func (g *MediaGrid) TypedRune(r rune) {}

// SetOnRefresh sets callback for refresh events
func (g *MediaGrid) SetOnRefresh(callback func()) {
	g.onRefresh = callback
}
