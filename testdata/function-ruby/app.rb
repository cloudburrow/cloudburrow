# The Ruby fixture used to verify Google Functions Framework and Buildpacks
# support (#1319).
#
# It is an ordinary Functions Framework function: no CloudBurrow-specific
# code, because the point is that a normal function runs unchanged. It
# answers as testdata/function does.
require "functions_framework"
require "json"

# hello answers an HTTP request, echoing a name when given one.
FunctionsFramework.http "hello" do |request|
  body = begin
    JSON.parse(request.body&.read.to_s)
  rescue JSON::ParserError
    {}
  end
  name = body.is_a?(Hash) && !body["name"].to_s.empty? ? body["name"] : "world"
  { greeting: "hello #{name}" }
end

# event receives a CloudEvent in the Google schema and logs it.
FunctionsFramework.cloud_event "event" do |event|
  data = event.data
  data = JSON.generate(data) unless data.is_a?(String)
  puts "received CloudEvent type=#{event.type} subject=#{event.subject} data=#{data}"
  $stdout.flush
end
