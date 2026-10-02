-- Recording-only: show the directory name rather than a machine-specific path.
-- https://github.com/sxyazi/yazi/blob/v26.9.1/yazi-plugin/preset/components/header.lua
Header:children_remove(1, Header.LEFT)
Header:children_add(function(self)
  return ui.Span(self._current.cwd.name .. self:flags()):style(th.mgr.cwd)
end, 1000, Header.LEFT)
