package media

import "fmt"

// DisplayTitle renders "Title (Year)", or just the title when the year is
// unknown.
func DisplayTitle(title string, year int) string {
	if year > 0 {
		return fmt.Sprintf("%s (%d)", title, year)
	}
	return title
}
