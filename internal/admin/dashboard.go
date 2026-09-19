package admin

import "github.com/iaia/telegramgw/internal/registry"

func (s *Server) redactDashboard(d *registry.AdminDashboard) {
	if s.redactor == nil {
		return
	}
	redact := s.redactor.Redact
	for i := range d.Sessions {
		session := &d.Sessions[i]
		session.Name, session.Preview = redact(session.Name), redact(session.Preview)
		session.CWD, session.GitBranch, session.GitRoot = redact(session.CWD), redact(session.GitBranch), redact(session.GitRoot)
		session.WorkerName, session.RuntimeName = redact(session.WorkerName), redact(session.RuntimeName)
		if session.Stats != nil {
			session.Stats.LastMessage = redact(session.Stats.LastMessage)
			session.Stats.Model = redact(session.Stats.Model)
			session.Stats.ReasoningEffort = redact(session.Stats.ReasoningEffort)
		}
	}
	for i := range d.Workers {
		d.Workers[i].Name = redact(d.Workers[i].Name)
		d.Workers[i].Hostname = redact(d.Workers[i].Hostname)
	}
	for i := range d.Runtimes {
		d.Runtimes[i].Name = redact(d.Runtimes[i].Name)
		d.Runtimes[i].ProfileID = redact(d.Runtimes[i].ProfileID)
		d.Runtimes[i].DefaultCWD = redact(d.Runtimes[i].DefaultCWD)
		d.Runtimes[i].LocalSocket = redact(d.Runtimes[i].LocalSocket)
	}
}
