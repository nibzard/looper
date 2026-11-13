package looper

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/nibzard/looper/internal/ui"
)

// A viewer follows replacement controllers. Actions use the last displayed
// session, not a replacement that the user has not seen yet.
type dashboardAttachment struct {
	mu           sync.Mutex
	observer     *observer
	dir, session string
}

func (a *dashboardAttachment) poll() *ui.ViewSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	info, err := readController(a.dir)
	if err != nil {
		return nil
	}
	v := a.observer.view(a.dir, info.Session)
	if v != nil {
		a.session = v.Session
	}
	return v
}

func (a *dashboardAttachment) currentSession() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	info, err := readController(a.dir)
	if err != nil || info.Session != a.session || !dashboardControllerAlive(info) {
		return "", fmt.Errorf("The controller changed or disconnected. Wait for the dashboard to reconnect, then retry.")
	}
	return a.session, nil
}

// addPlan submits a plan import on behalf of an attached viewer and returns
// the request identifier, so the dashboard can name the queued import. The
// published snapshot carries its later states; no event parsing is involved.
func (a *dashboardAttachment) addPlan(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	session, err := a.currentSession()
	if err != nil {
		return "", err
	}
	return submitPlanImport(a.dir, session, "", []string{absolute})
}

func (a *dashboardAttachment) inspect(id string) (ui.TaskDetail, error) {
	session, err := a.currentSession()
	if err != nil {
		return ui.TaskDetail{}, err
	}
	detail, err := a.observer.inspect(id)
	if err != nil {
		return detail, err
	}
	current, err := a.currentSession()
	if err != nil || current != session {
		return ui.TaskDetail{}, fmt.Errorf("The controller changed while reading the task. Wait for the next refresh.")
	}
	return detail, nil
}
