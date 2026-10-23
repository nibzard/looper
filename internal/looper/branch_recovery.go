package looper

import (
	"fmt"
	"strings"
)

// recoverCommittedHead accepts a new commit only during startup, under the
// repository lock. Publication still performs its normal contract and tree checks.
func (e *Engine) recoverCommittedHead() error {
	head, branch := e.Repo.Head()
	if head == e.State.Head && branch == e.State.Branch {
		return nil
	}
	fail := func() error {
		return fmt.Errorf("the project branch moved; retained work is in %s. Resume requires the same branch and unchanged accepted project content", e.Dir)
	}
	if head == "" || branch == "" || branch != e.State.Branch {
		return fail()
	}
	if _, err := gitText(e.Config.Root, "merge-base", "--is-ancestor", e.State.Head, head); err != nil {
		return fail()
	}
	trees := []string{e.State.Published}
	if p := e.State.Publication; p != nil {
		trees = append(trees, p.Tree)
	}
	paths := []string{"--", "."}
	for _, path := range e.Repo.Excluded {
		paths = append(paths, ":(exclude)"+path)
	}
	staged, err := gitText(e.Config.Root, append([]string{"diff", "--cached", "--name-only", head}, paths...)...)
	if err != nil || staged != "" {
		return fail()
	}
	matched := false
	for _, tree := range trees {
		changed, err := gitText(e.Config.Root, append([]string{"diff", "--name-only", head, tree}, paths...)...)
		if err != nil || strings.TrimSpace(changed) != "" {
			continue
		}
		observed, err := e.Repo.SnapshotFrom(e.Config.Root, tree, trees...)
		if err == nil && observed == tree {
			matched = true
			break
		}
	}
	if !matched {
		return fail()
	}
	// Do not accept a ref that moved during the comparison.
	current, currentBranch := e.Repo.Head()
	if current != head || currentBranch != branch {
		return fail()
	}
	old := e.State.Head
	e.State.Head = head
	if err := e.save(); err != nil {
		e.State.Head = old
		return err
	}
	e.eventData("recovery", "Accepted a new commit with unchanged project content; resuming retained work.", map[string]string{"previous_head": old, "head": head})
	return e.err
}
