"""Pub/Sub through the official google-cloud-pubsub client, configured by
PUBSUB_EMULATOR_HOST alone."""

from google.cloud import pubsub_v1


def test_publish_and_pull_with_ack(project, suffix):
    publisher = pubsub_v1.PublisherClient()
    subscriber = pubsub_v1.SubscriberClient()
    topic = publisher.topic_path(project, f"py-topic-{suffix}")
    sub = subscriber.subscription_path(project, f"py-sub-{suffix}")
    publisher.create_topic(name=topic)
    subscriber.create_subscription(name=sub, topic=topic)
    try:
        message_id = publisher.publish(topic, b"ping", origin="python").result(timeout=30)
        assert message_id

        received = subscriber.pull(subscription=sub, max_messages=1, timeout=30).received_messages
        assert len(received) == 1
        assert received[0].message.data == b"ping"
        assert received[0].message.attributes["origin"] == "python"
        subscriber.acknowledge(subscription=sub, ack_ids=[received[0].ack_id])

        # Acknowledged, so a second pull finds nothing to redeliver.
        again = subscriber.pull(subscription=sub, max_messages=1, return_immediately=True, timeout=10)
        assert not again.received_messages
    finally:
        subscriber.delete_subscription(subscription=sub)
        publisher.delete_topic(topic=topic)
