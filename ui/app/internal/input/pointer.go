// SPDX-License-Identifier: Unlicense OR MIT

package input

import (
	"encoding/binary"
	"image"

	"gioui.org/ui"
	"gioui.org/ui/f32"
	"gioui.org/ui/input"
	"gioui.org/ui/internal/ops"
	"gioui.org/ui/pointer"
)

type pointerQueue struct {
	hitTree  []hitNode
	areas    []areaNode
	handlers map[input.Key]*pointerHandler
	pointers []pointerInfo
	reader   ui.OpsReader
	scratch  []input.Key
}

type hitNode struct {
	next int
	area int
	// Pass tracks the most recent PassOp mode.
	pass bool

	// For handler nodes.
	key input.Key
}

type pointerInfo struct {
	id       pointer.ID
	pressed  bool
	handlers []input.Key
}

type pointerHandler struct {
	area      int
	active    bool
	transform ui.Transform
	wantsGrab bool
}

type areaOp struct {
	kind areaKind
	size image.Point
}

type areaNode struct {
	trans ui.Transform
	next  int
	area  areaOp
}

type areaKind uint8

const (
	areaRect areaKind = iota
	areaEllipse
)

func (q *pointerQueue) collectHandlers(r *ui.OpsReader, events *handlerEvents, t ui.Transform, area, node int, pass bool) {
	for encOp, ok := r.Decode(); ok; encOp, ok = r.Decode() {
		switch ops.OpType(encOp.Data[0]) {
		case ops.TypePush:
			q.collectHandlers(r, events, t, area, node, pass)
		case ops.TypePop:
			return
		case ops.TypePass:
			var op pointer.PassOp
			op.Decode(encOp.Data)
			pass = op.Pass
		case ops.TypeArea:
			var op areaOp
			op.Decode(encOp.Data)
			q.areas = append(q.areas, areaNode{trans: t, next: area, area: op})
			area = len(q.areas) - 1
			q.hitTree = append(q.hitTree, hitNode{
				next: node,
				area: area,
				pass: pass,
			})
			node = len(q.hitTree) - 1
		case ops.TypeTransform:
			var op ui.TransformOp
			op.Decode(encOp.Data)
			t = t.Mul(op.Transform)
		case ops.TypePointerHandler:
			var op pointer.HandlerOp
			op.Decode(encOp.Data, encOp.Refs)
			q.hitTree = append(q.hitTree, hitNode{
				next: node,
				area: area,
				pass: pass,
				key:  op.Key,
			})
			node = len(q.hitTree) - 1
			h, ok := q.handlers[op.Key]
			if !ok {
				h = new(pointerHandler)
				q.handlers[op.Key] = h
				events.Set(op.Key, []input.Event{pointer.Event{Type: pointer.Cancel}})
			}
			h.active = true
			h.area = area
			h.transform = t
			h.wantsGrab = h.wantsGrab || op.Grab
		}
	}
}

func (q *pointerQueue) opHit(handlers *[]input.Key, pos f32.Point) {
	// Track whether we're passing through hits.
	pass := true
	idx := len(q.hitTree) - 1
	for idx >= 0 {
		n := &q.hitTree[idx]
		if !q.hit(n.area, pos) {
			idx--
			continue
		}
		pass = pass && n.pass
		if pass {
			idx--
		} else {
			idx = n.next
		}
		if n.key != nil {
			if _, exists := q.handlers[n.key]; exists {
				*handlers = append(*handlers, n.key)
			}

		}
	}
}

func (q *pointerQueue) hit(areaIdx int, p f32.Point) bool {
	for areaIdx != -1 {
		a := &q.areas[areaIdx]
		if !a.hit(p) {
			return false
		}
		areaIdx = a.next
	}
	return true
}

func (a *areaNode) hit(p f32.Point) bool {
	p = a.trans.InvTransform(p)
	return a.area.Hit(p)
}

func (q *pointerQueue) init() {
	if q.handlers == nil {
		q.handlers = make(map[input.Key]*pointerHandler)
	}
}

func (q *pointerQueue) Frame(root *ui.Ops, events *handlerEvents) {
	q.init()
	for _, h := range q.handlers {
		// Reset handler.
		h.active = false
	}
	q.hitTree = q.hitTree[:0]
	q.areas = q.areas[:0]
	q.reader.Reset(root)
	q.collectHandlers(&q.reader, events, ui.Transform{}, -1, -1, false)
	for k, h := range q.handlers {
		if !h.active {
			q.dropHandler(k)
		}
	}
}

func (q *pointerQueue) dropHandler(k input.Key) {
	delete(q.handlers, k)
	for i := range q.pointers {
		p := &q.pointers[i]
		for i := len(p.handlers) - 1; i >= 0; i-- {
			if p.handlers[i] == k {
				p.handlers = append(p.handlers[:i], p.handlers[i+1:]...)
			}
		}
	}
}

func (q *pointerQueue) Push(e pointer.Event, events *handlerEvents) {
	q.init()
	if e.Type == pointer.Cancel {
		q.pointers = q.pointers[:0]
		for k := range q.handlers {
			q.dropHandler(k)
		}
		return
	}
	pidx := -1
	for i, p := range q.pointers {
		if p.id == e.PointerID {
			pidx = i
			break
		}
	}
	if pidx == -1 {
		q.pointers = append(q.pointers, pointerInfo{id: e.PointerID})
		pidx = len(q.pointers) - 1
	}
	p := &q.pointers[pidx]
	if !p.pressed && (e.Type == pointer.Move || e.Type == pointer.Press) {
		p.handlers, q.scratch = q.scratch[:0], p.handlers
		q.opHit(&p.handlers, e.Position)
		// Drop handlers no longer hit.
	loop:
		for _, h := range q.scratch {
			for _, h2 := range p.handlers {
				if h == h2 {
					continue loop
				}
			}
			q.dropHandler(h)
		}
		if e.Type == pointer.Press {
			p.pressed = true
		}
	}
	if p.pressed {
		// Resolve grabs.
		q.scratch = q.scratch[:0]
		for i, k := range p.handlers {
			h := q.handlers[k]
			if h.wantsGrab {
				q.scratch = append(q.scratch, p.handlers[:i]...)
				q.scratch = append(q.scratch, p.handlers[i+1:]...)
				break
			}
		}
		// Drop handlers that lost their grab.
		for _, k := range q.scratch {
			q.dropHandler(k)
		}
	}
	if e.Type == pointer.Release {
		q.pointers = append(q.pointers[:pidx], q.pointers[pidx+1:]...)
	}
	for i, k := range p.handlers {
		h := q.handlers[k]
		e := e
		switch {
		case p.pressed && len(p.handlers) == 1:
			e.Priority = pointer.Grabbed
		case i == 0:
			e.Priority = pointer.Foremost
		}
		e.Hit = q.hit(h.area, e.Position)
		e.Position = h.transform.InvTransform(e.Position)
		events.Add(k, e)
		if e.Type == pointer.Release {
			// Release grab when the number of grabs reaches zero.
			grabs := 0
			for _, p := range q.pointers {
				if p.pressed && len(p.handlers) == 1 && p.handlers[0] == k {
					grabs++
				}
			}
			if grabs == 0 {
				h.wantsGrab = false
			}
		}
	}
}

func (op *areaOp) Decode(d []byte) {
	if ops.OpType(d[0]) != ops.TypeArea {
		panic("invalid op")
	}
	bo := binary.LittleEndian
	size := image.Point{
		X: int(bo.Uint32(d[2:])),
		Y: int(bo.Uint32(d[6:])),
	}
	*op = areaOp{
		kind: areaKind(d[1]),
		size: size,
	}
}

func (op *areaOp) Hit(pos f32.Point) bool {
	switch op.kind {
	case areaRect:
		if 0 <= pos.X && pos.X < float32(op.size.X) &&
			0 <= pos.Y && pos.Y < float32(op.size.Y) {
			return true
		} else {
			return false
		}
	case areaEllipse:
		rx := float32(op.size.X) / 2
		ry := float32(op.size.Y) / 2
		rx2 := rx * rx
		ry2 := ry * ry
		xh := pos.X - rx
		yk := pos.Y - ry
		if xh*xh*ry2+yk*yk*rx2 <= rx2*ry2 {
			return true
		} else {
			return false
		}
	default:
		panic("invalid area kind")
	}
}
