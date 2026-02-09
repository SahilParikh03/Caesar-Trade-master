package adapter

import (
	"context"
	"log"
	"sync"
)

// UpdatesProvider is the interface that exchange adapters must satisfy
// to plug into the Broadcaster.
type UpdatesProvider interface {
	Updates() <-chan BookUpdate
}

// subKey identifies a filtered subscription by exchange and market.
type subKey struct {
	Exchange Exchange
	MarketID string
}

// Broadcaster is a many-to-many hub that ingests BookUpdates from any number
// of exchange adapters and distributes them to filtered subscribers and a
// unified "all" stream.
type Broadcaster struct {
	sources []<-chan BookUpdate

	// Filtered subscribers keyed by (exchange, marketID).
	mu   sync.RWMutex
	subs map[subKey][]chan BookUpdate

	// allMu guards the unified subscriber list.
	allMu  sync.RWMutex
	allSub []chan BookUpdate
}

// NewBroadcaster creates a Broadcaster ready for adapter registration.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{
		subs: make(map[subKey][]chan BookUpdate),
	}
}

// Register adds an adapter's update channel as a source. Must be called
// before Run.
func (b *Broadcaster) Register(provider UpdatesProvider) {
	b.sources = append(b.sources, provider.Updates())
}

// Subscribe returns a buffered channel that receives BookUpdates for the
// given exchange and market. The caller must drain the channel to avoid
// dropped messages.
func (b *Broadcaster) Subscribe(exchange Exchange, marketID string) <-chan BookUpdate {
	ch := make(chan BookUpdate, 256)
	key := subKey{Exchange: exchange, MarketID: marketID}

	b.mu.Lock()
	b.subs[key] = append(b.subs[key], ch)
	b.mu.Unlock()

	return ch
}

// SubscribeAll returns a buffered channel that receives every BookUpdate
// regardless of exchange or market. Intended for logging, metrics, or
// persistence (e.g. Redis in Ticket 2.6).
func (b *Broadcaster) SubscribeAll() <-chan BookUpdate {
	ch := make(chan BookUpdate, 512)

	b.allMu.Lock()
	b.allSub = append(b.allSub, ch)
	b.allMu.Unlock()

	return ch
}

// Run starts consuming from all registered sources and distributing updates.
// It blocks until ctx is cancelled. Each source gets its own goroutine.
func (b *Broadcaster) Run(ctx context.Context) {
	var wg sync.WaitGroup

	for _, src := range b.sources {
		wg.Add(1)
		go func(ch <-chan BookUpdate) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case update, ok := <-ch:
					if !ok {
						return
					}
					b.distribute(update)
				}
			}
		}(src)
	}

	wg.Wait()
}

// distribute sends an update to all matching filtered subscribers and all
// unified subscribers. Non-blocking: slow consumers get messages dropped.
func (b *Broadcaster) distribute(update BookUpdate) {
	key := subKey{Exchange: update.Exchange, MarketID: update.MarketID}

	b.mu.RLock()
	if subs, ok := b.subs[key]; ok {
		for _, ch := range subs {
			select {
			case ch <- update:
			default:
				log.Printf("broadcaster: dropping update for slow subscriber (%s/%s)",
					update.Exchange, update.MarketID)
			}
		}
	}
	b.mu.RUnlock()

	b.allMu.RLock()
	for _, ch := range b.allSub {
		select {
		case ch <- update:
		default:
			// Slow unified subscriber — drop.
		}
	}
	b.allMu.RUnlock()
}
