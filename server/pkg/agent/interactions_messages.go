package agent

import "sync"

// controlMessagePublisher decouples provider readers from a slow Session
// consumer. Transcript events still use the established best-effort channel,
// while control records enter this bounded queue. A full queue is observable to
// the controller, which closes admission and rejects provider requests rather
// than silently losing an approval or question.
type controlMessagePublisher struct {
	mu     sync.Mutex
	closed bool
	queue  chan Message
	stop   chan struct{}
	done   chan struct{}
}

func newControlMessagePublisher(out chan<- Message) *controlMessagePublisher {
	return newControlMessagePublisherWithCapacity(out, 64)
}

func newControlMessagePublisherWithCapacity(out chan<- Message, capacity int) *controlMessagePublisher {
	if capacity < 1 {
		capacity = 1
	}
	publisher := &controlMessagePublisher{
		queue: make(chan Message, capacity),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go func() {
		defer close(publisher.done)
		for {
			select {
			case <-publisher.stop:
				return
			case message := <-publisher.queue:
				select {
				case <-publisher.stop:
					return
				case out <- message:
				}
			}
		}
	}()
	return publisher
}

func (p *controlMessagePublisher) publish(message Message) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	select {
	case p.queue <- message:
		return true
	default:
		return false
	}
}

func (p *controlMessagePublisher) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.stop)
	p.mu.Unlock()
	<-p.done
}
