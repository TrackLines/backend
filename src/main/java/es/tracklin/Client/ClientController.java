package es.tracklin.Client;

import es.tracklin.Configuration.MongoConfiguration;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Controller;
import org.springframework.web.bind.annotation.RequestBody;
import org.springframework.web.bind.annotation.RequestMapping;
import org.springframework.web.bind.annotation.RequestMethod;
import org.springframework.web.bind.annotation.ResponseBody;

@Controller
@RequestMapping("/v1/client")
public class ClientController {
    private MongoConfiguration mongoConfiguration;

    @Autowired
    public void setMongoConfiguration(MongoConfiguration mongoConfiguration) {
        this.mongoConfiguration = mongoConfiguration;
    }


    @RequestMapping(method = RequestMethod.POST)
    public @ResponseBody Client login(@RequestBody LoginModel loginModel) {
        Client client = new Client(this.mongoConfiguration);
        client.login(loginModel);

        return client;
    }

    @RequestMapping(method = RequestMethod.GET)
    public @ResponseBody Client getData() {
        return new Client(this.mongoConfiguration);
    }
}
