// Package frame configures terminal frames around child commands.
// Regions draw into borrowed, region-sized uv.Screen canvases through typed
// drawing contexts; Lip Gloss styles and layers are optional.
// A controller accepts region payload invalidations independently of geometry.
package frame
