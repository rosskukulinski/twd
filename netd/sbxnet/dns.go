package sbxnet

import (
	"golang.org/x/net/dns/dnsmessage"
)

// DNS answers one guest DNS query (wire format). Only A queries for names
// the container may reach get an answer (a synthetic address, TTL 1 s, so
// an intercept registered later is seen at once); AAAA for such a name is
// an empty NOERROR (IPv4 only); everything else is NXDOMAIN or REFUSED.
// nil means drop (unparseable).
func (p *Proxy) DNS(query []byte) []byte {
	var parser dnsmessage.Parser
	h, err := parser.Start(query)
	if err != nil || h.Response {
		return nil
	}
	q, err := parser.Question()
	if err != nil {
		return nil
	}
	rh := dnsmessage.Header{ID: h.ID, Response: true, OpCode: h.OpCode, RecursionDesired: h.RecursionDesired,
		RecursionAvailable: true, Authoritative: true}
	reply := func(code dnsmessage.RCode, answer bool, a [4]byte) []byte {
		rh.RCode = code
		b := dnsmessage.NewBuilder(make([]byte, 0, 512), rh)
		b.EnableCompression()
		if b.StartQuestions() != nil || b.Question(q) != nil {
			return nil
		}
		if answer {
			if b.StartAnswers() != nil {
				return nil
			}
			if b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 1}, dnsmessage.AResource{A: a}) != nil {
				return nil
			}
		}
		out, err := b.Finish()
		if err != nil {
			return nil
		}
		return out
	}
	if h.OpCode != 0 || q.Class != dnsmessage.ClassINET {
		return reply(dnsmessage.RCodeRefused, false, [4]byte{})
	}
	addr, ok := p.Lookup(q.Name.String())
	if !ok {
		p.deny(q.Name.String(), "dns_refused", "")
		return reply(dnsmessage.RCodeNameError, false, [4]byte{})
	}
	if q.Type == dnsmessage.TypeA {
		return reply(dnsmessage.RCodeSuccess, true, addr.As4())
	}
	return reply(dnsmessage.RCodeSuccess, false, [4]byte{})
}
