package es.tracklin.Configuration;

import org.springframework.boot.context.properties.ConfigurationProperties;
import org.springframework.context.annotation.Configuration;

import java.security.MessageDigest;
import java.util.Date;

@Configuration
@ConfigurationProperties(prefix = "tokens")
public class Tokens {
    private String web;
    private String api;
    private int length;

    public String getWeb() {
        return web;
    }

    public void setWeb(String web) {
        this.web = web;
    }

    public String getApi() {
        return api;
    }

    public void setApi(String api) {
        this.api = api;
    }

    public int getLength() {
        return length;
    }

    public void setLength(int length) {
        this.length = length;
    }

    public String generateToken(String input, String tokenName) {
        String returnData = "";

        returnData = (returnData + tokenName);
        returnData = (returnData + "_" + new Date());
        returnData = (returnData + "_" + input);
        returnData = getSha(returnData);

        return returnData;
    }

    private String getSha(String input) {
        try{
            MessageDigest digest = MessageDigest.getInstance("SHA-256");
            byte[] hash = digest.digest(input.getBytes("UTF-8"));
            StringBuilder hexString = new StringBuilder();

            for (byte hashItem : hash) {
                String hex = Integer.toHexString(0xff & hashItem);
                if(hex.length() == 1) hexString.append('0');
                hexString.append(hex);
            }

            return hexString.toString();
        } catch(Exception ex){
            throw new RuntimeException(ex);
        }
    }
}
