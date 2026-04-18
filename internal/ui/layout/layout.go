package layout

// Rect represents a rectangular area in the terminal.
type Rect struct {
	X, Y, W, H int
}

// Layout defines the layout structure for the SelfMind TUI.
type Layout struct {
	Header   Rect
	Sidebar  Rect
	Main     Rect
	Input    Rect
	Status   Rect
}

// CalculateLayout computes the layout based on terminal dimensions.
func CalculateLayout(width, height int) Layout {
	// Sidebar removed for maximum efficiency.
	return Layout{
		Header:  Rect{0, 0, width, 1},
		Sidebar: Rect{0, 0, 0, 0},
		Main:    Rect{0, 1, width, height - 5},
		Input:   Rect{0, height - 4, width, 3},
		Status:  Rect{0, height - 1, width, 1},
	}
}
