package producer

import "strings"

func (p *Producer) topic2Stream(topic string) string {
	queueNamespace := append(p.namespaces, "Q")
	return strings.Join(append(queueNamespace, topic), ":")
}

func (p *Producer) stream2Topic(stream string) string {
	queueNamespace := append(p.namespaces, "Q")
	return strings.TrimPrefix(stream, strings.Join(queueNamespace, ":")+":")
}
