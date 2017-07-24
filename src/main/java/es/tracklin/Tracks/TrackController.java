package es.tracklin.Tracks;

import org.springframework.stereotype.Controller;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RequestMethod;
import org.springframework.web.bind.annotation.RequestParam;
import org.springframework.web.bind.annotation.ResponseBody;

import java.util.concurrent.atomic.AtomicLong;

@Controller
@RequestMapping("/v1/track")
public class TrackController {
    private static final String template = "Hello %s";
    private final AtomicLong counter = new AtomicLong();

    @RequestMapping(method = RequestMethod.GET)
    public @ResponseBody Track sayHello(@RequestParam(value = "name", required = false, defaultValue = "Test") String name) {
        return new Track(counter.incrementAndGet(), String.format(template, name));
    }
}
