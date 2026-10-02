# Push changes into the frame

An application event submits new data:

app.InvalidateHeader(nextStatus)

The header redraws while Yazi is idle.
Pending updates coalesce.
There is no periodic refresh timer.
