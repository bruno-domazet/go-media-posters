package tree

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charlievieth/fastwalk"
	"github.com/jellydator/ttlcache/v3"
)

// Node represents a file or directory in the tree
type Node struct {
	Name       string
	Path       string
	IsDir      bool
	Children   []*Node
	Parent     *Node
	mu         sync.RWMutex
	IsVideo    bool   // True if this is a video file
	PosterPath string // Path to poster image if available
}

// Tree holds the file tree structure
type Tree struct {
	Root         *Node
	CurrentDir   *Node
	SelectedIdx  int
	VisibleNodes []*Node
	nodeMap      map[string]*Node
	mu           sync.RWMutex
	isLoading    bool
	filesFound   atomic.Int64
	dirsFound    atomic.Int64
	cache        *ttlcache.Cache[string, []*Node]
	filter       *Filter
}

// Filter defines file/directory filtering rules
type Filter struct {
	SkipHidden      bool
	SkipExtensions  map[string]bool
	MaxChildrenShow int // Max children to show per directory for performance
}

// DefaultFilter returns a filter that skips common unwanted files
func DefaultFilter() *Filter {
	return &Filter{
		SkipHidden: true,
		SkipExtensions: map[string]bool{
			".nfo": true,
			".png": true,
		},
		MaxChildrenShow: 1000, // Limit displayed items for performance
	}
}

// IsVideoFile checks if a file is a video based on extension
func IsVideoFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".mkv" || ext == ".avi" || ext == ".mp4" || ext == ".m4v"
}

// IsPosterFile checks if a file is a poster image
func IsPosterFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	if ext != ".jpg" && ext != ".jpeg" {
		return false
	}
	return strings.HasSuffix(strings.ToLower(name), "-poster.jpg") ||
		strings.HasSuffix(strings.ToLower(name), "-poster.jpeg")
}

// ShouldSkip checks if a file/directory should be skipped
func (f *Filter) ShouldSkip(name string, isDir bool) bool {
	// Skip hidden files/directories
	if f.SkipHidden && strings.HasPrefix(name, ".") {
		return true
	}

	// Skip by extension (only for files)
	if !isDir {
		ext := strings.ToLower(filepath.Ext(name))
		if f.SkipExtensions[ext] {
			return true
		}
	}

	return false
}

// LoadAsync loads the directory structure asynchronously using fastwalk with TTL caching
// It recursively traverses the entire tree structure upfront for optimal navigation performance
func LoadAsync(rootPath string, filter *Filter, onProgress func(files, dirs int64), onComplete func()) (*Tree, error) {
	info, err := os.Stat(rootPath)
	if err != nil {
		return nil, err
	}

	if filter == nil {
		filter = DefaultFilter()
	}

	root := &Node{
		Name:     filepath.Base(rootPath),
		Path:     rootPath,
		IsDir:    info.IsDir(),
		Children: make([]*Node, 0),
		Parent:   nil,
	}

	// Create TTL cache with 5 minute expiration
	cache := ttlcache.New[string, []*Node](
		ttlcache.WithTTL[string, []*Node](5 * time.Minute),
	)

	// Start automatic expired item deletion
	go cache.Start()

	tree := &Tree{
		Root:        root,
		CurrentDir:  root,
		SelectedIdx: 0,
		nodeMap:     make(map[string]*Node),
		isLoading:   true,
		cache:       cache,
		filter:      filter,
	}

	// Add root to map
	tree.nodeMap[rootPath] = root

	// Recursively load entire tree structure in background
	go func() {
		log.Printf("Starting recursive tree traversal from: %s", rootPath)
		children := tree.loadDirectory(root, true) // recursive=true
		root.mu.Lock()
		root.Children = children
		root.mu.Unlock()

		tree.mu.Lock()
		tree.isLoading = false
		tree.mu.Unlock()

		tree.UpdateVisibleNodes()

		log.Printf("Tree traversal complete: %d files, %d dirs", tree.filesFound.Load(), tree.dirsFound.Load())

		if onComplete != nil {
			onComplete()
		}

		if onProgress != nil {
			onProgress(tree.filesFound.Load(), tree.dirsFound.Load())
		}
	}()

	tree.UpdateVisibleNodes()
	return tree, nil
}

// loadDirectory loads the children of a directory, using cache if available
// If recursive=true, it will traverse all subdirectories and pre-load the entire tree
func (t *Tree) loadDirectory(node *Node, recursive bool) []*Node {
	if !node.IsDir {
		return nil
	}

	// Check cache first
	if item := t.cache.Get(node.Path); item != nil {
		log.Printf("Cache hit for: %s", node.Path)
		return item.Value()
	}

	log.Printf("Loading directory: %s (recursive=%v)", node.Path, recursive)

	children := make([]*Node, 0)
	posterMap := make(map[string]string) // basename -> poster path
	var childrenMu sync.Mutex

	// Use fastwalk for faster directory scanning (non-recursive)
	conf := fastwalk.Config{
		Follow:     false,
		NumWorkers: 1,                      // Single directory, no need for parallelism
		MaxDepth:   1,                      // Only immediate children
		Sort:       fastwalk.SortDirsFirst, // Sort entries for consistent order
	}

	err := fastwalk.Walk(&conf, node.Path, func(path string, de os.DirEntry, err error) error {
		if err != nil {
			return nil // Continue on errors
		}

		// Skip the root directory itself
		if path == node.Path {
			return nil
		}

		// Only process immediate children
		if filepath.Dir(path) != node.Path {
			if de.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		name := de.Name()

		// Apply filter
		if t.filter.ShouldSkip(name, de.IsDir()) {
			if de.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Collect poster files
		if IsPosterFile(name) {
			base := strings.TrimSuffix(strings.TrimSuffix(name, "-poster.jpg"), "-poster.jpeg")
			childrenMu.Lock()
			posterMap[base] = path
			childrenMu.Unlock()
			if de.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		child := &Node{
			Name:     name,
			Path:     path,
			IsDir:    de.IsDir(),
			Children: make([]*Node, 0),
			Parent:   node,
			IsVideo:  !de.IsDir() && IsVideoFile(name),
		}

		childrenMu.Lock()
		children = append(children, child)
		childrenMu.Unlock()

		// Add to node map
		t.mu.Lock()
		t.nodeMap[path] = child
		t.mu.Unlock()

		// Update counters
		if de.IsDir() {
			t.dirsFound.Add(1)
		} else {
			t.filesFound.Add(1)
		}

		// Don't recurse into subdirectories
		if de.IsDir() {
			return filepath.SkipDir
		}

		return nil
	})

	if err != nil {
		log.Printf("Error walking directory %s: %v", node.Path, err)
	}

	// Associate posters with children (no additional ReadDir needed)
	t.associatePosters(node.Path, children, posterMap)

	// Recursively load subdirectories if requested
	if recursive {
		for _, child := range children {
			if child.IsDir {
				subChildren := t.loadDirectory(child, true)
				child.mu.Lock()
				child.Children = subChildren
				child.mu.Unlock()
			}
		}
	}

	// Store in cache
	t.cache.Set(node.Path, children, ttlcache.DefaultTTL)

	return children
}

// associatePosters finds and associates poster images with videos or directories
func (t *Tree) associatePosters(dirPath string, children []*Node, posterMap map[string]string) {
	// Associate posters with children
	for _, child := range children {
		// Get basename without extension
		baseName := strings.TrimSuffix(child.Name, filepath.Ext(child.Name))

		if posterPath, exists := posterMap[baseName]; exists {
			child.PosterPath = posterPath
		} else if child.IsDir {
			// For directories without a poster in parent, check if the directory name matches
			if posterPath, exists := posterMap[child.Name]; exists {
				child.PosterPath = posterPath
			}
			// Note: We don't peek inside directories here for performance
			// Posters will be discovered when entering the directory
		}
	}
}

// UpdateVisibleNodes updates the list of nodes visible in the current directory
func (t *Tree) UpdateVisibleNodes() {
	t.CurrentDir.mu.RLock()
	t.VisibleNodes = make([]*Node, len(t.CurrentDir.Children))
	copy(t.VisibleNodes, t.CurrentDir.Children)
	t.CurrentDir.mu.RUnlock()

	// Reset selection if out of bounds
	if t.SelectedIdx >= len(t.VisibleNodes) {
		t.SelectedIdx = len(t.VisibleNodes) - 1
	}
	if t.SelectedIdx < 0 && len(t.VisibleNodes) > 0 {
		t.SelectedIdx = 0
	}
}

// NavigateUp moves selection up (by columns)
func (t *Tree) NavigateUp(cols int) {
	if t.SelectedIdx >= cols {
		t.SelectedIdx -= cols
	}
}

// NavigateDown moves selection down (by columns)
func (t *Tree) NavigateDown(cols int) {
	newIdx := t.SelectedIdx + cols
	if newIdx < len(t.VisibleNodes) {
		t.SelectedIdx = newIdx
	}
}

// NavigateLeft moves selection left
func (t *Tree) NavigateLeft() {
	if t.SelectedIdx > 0 {
		t.SelectedIdx--
	}
}

// NavigateRight moves selection right
func (t *Tree) NavigateRight() {
	if t.SelectedIdx < len(t.VisibleNodes)-1 {
		t.SelectedIdx++
	}
}

// Enter navigates into the selected directory
func (t *Tree) Enter() {
	if len(t.VisibleNodes) == 0 {
		return
	}

	selected := t.VisibleNodes[t.SelectedIdx]

	// Only enter if it's a directory
	if !selected.IsDir {
		log.Printf("Selected file: %s", selected.Path)
		return
	}

	// Tree is pre-loaded recursively, but check cache just in case
	// This ensures cache expiry is properly handled and provides fallback
	children := t.loadDirectory(selected, false) // non-recursive for on-demand loading
	selected.mu.Lock()
	selected.Children = children
	selected.mu.Unlock()

	t.CurrentDir = selected
	t.SelectedIdx = 0
	t.UpdateVisibleNodes()
}

// GoUp navigates to parent directory
func (t *Tree) GoUp() {
	if t.CurrentDir.Parent != nil {
		t.CurrentDir = t.CurrentDir.Parent
		t.SelectedIdx = 0
		t.UpdateVisibleNodes()
	}
}

// IsLoading returns whether the tree is still loading
func (t *Tree) IsLoading() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.isLoading
}

// GetStats returns the current file and directory counts
func (t *Tree) GetStats() (files, dirs int64) {
	return t.filesFound.Load(), t.dirsFound.Load()
}
