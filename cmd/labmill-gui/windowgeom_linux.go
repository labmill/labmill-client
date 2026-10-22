//go:build desktop && linux

package main

/*
#cgo pkg-config: gtk+-3.0
#include <gtk/gtk.h>
#include <string.h>

typedef struct {
	int x, y;
	int done, abandoned;
	GMutex m;
	GCond c;
} moveJob;

// Runs on the GTK main thread: moves the "labmill" toplevel to root (desktop)
// coordinates, the same ones gtk_window_get_position (Wails' WindowGetPosition)
// reports. Wails' own WindowSetPosition adds the current monitor's origin.
static gboolean moveMain(gpointer data) {
	moveJob *j = (moveJob *)data;
	GList *l = gtk_window_list_toplevels();
	for (GList *it = l; it; it = it->next) {
		GtkWindow *w = GTK_WINDOW(it->data);
		const char *t = gtk_window_get_title(w);
		if (t && strcmp(t, "labmill") == 0) {
			gtk_window_move(w, j->x, j->y);
			break;
		}
	}
	g_list_free(l);
	g_mutex_lock(&j->m);
	j->done = 1;
	int drop = j->abandoned;
	g_cond_signal(&j->c);
	g_mutex_unlock(&j->m);
	if (drop) g_free(j);
	return G_SOURCE_REMOVE;
}

// moveWindow returns after the move ran or after 2 s (then the job is left
// to free itself).
static void moveWindow(int x, int y) {
	moveJob *j = g_new0(moveJob, 1);
	j->x = x;
	j->y = y;
	g_mutex_init(&j->m);
	g_cond_init(&j->c);
	g_idle_add(moveMain, j);
	gint64 end = g_get_monotonic_time() + 2 * G_TIME_SPAN_SECOND;
	g_mutex_lock(&j->m);
	while (!j->done && g_cond_wait_until(&j->c, &j->m, end)) {
	}
	int done = j->done;
	if (!done) j->abandoned = 1;
	g_mutex_unlock(&j->m);
	if (done) g_free(j);
}
*/
import "C"

import "context"

// windowSetPos moves the window to desktop position (x, y).
func windowSetPos(_ context.Context, x, y int) { C.moveWindow(C.int(x), C.int(y)) }
